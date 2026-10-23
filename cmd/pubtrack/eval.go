package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"pubtrack/internal/config"
	"pubtrack/internal/eval"
	"pubtrack/internal/llm"
	"pubtrack/internal/pubmed"
)

const defaultBenchModels = "llama3.2:1b,llama3.2:3b,llama3.1:8b"

// evalFlags are shared by eval and bench.
type evalFlags struct {
	configPath, dir, prompt, format *string
	threshold                       *int
}

func addEvalFlags(fl *flag.FlagSet) evalFlags {
	return evalFlags{
		configPath: fl.String("config", "config.json", "path to the config file"),
		dir:        fl.String("dir", "eval", "directory with pmids.txt and labels.csv"),
		prompt:     fl.String("prompt", "", "prompt version (default: the config's)"),
		format:     fl.String("format", "", `"schema" or "json" (default: the config's)`),
		threshold:  fl.Int("threshold", 4, "scores at or above this count as relevant"),
	}
}

// load reads the config with any overrides applied.
func (f evalFlags) load() (config.Config, error) {
	if *f.threshold < 1 || *f.threshold > 5 {
		return config.Config{}, fmt.Errorf("-threshold must be 1–5, got %d", *f.threshold)
	}
	cfg, err := config.Load(*f.configPath)
	if err != nil {
		return cfg, err
	}
	if *f.prompt != "" {
		cfg.PromptVersion = *f.prompt
	}
	if *f.format != "" {
		if *f.format != "schema" && *f.format != "json" {
			return cfg, fmt.Errorf(`-format must be "schema" or "json", got %q`, *f.format)
		}
		cfg.Ollama.Format = *f.format
	}
	return cfg, nil
}

type evalInputs struct {
	entries   []eval.Entry
	labels    map[string]eval.Label
	unlabeled int
	papers    map[string]pubmed.Article
}

func loadEvalInputs(ctx context.Context, dir string, cfg config.Config, stderr io.Writer) (evalInputs, error) {
	var in evalInputs
	var err error
	if in.entries, err = eval.ReadPMIDs(filepath.Join(dir, "pmids.txt")); err != nil {
		return in, err
	}
	labelsPath := filepath.Join(dir, "labels.csv")
	if in.labels, in.unlabeled, err = eval.ReadLabels(labelsPath); err != nil {
		return in, err
	}
	if len(in.labels) == 0 {
		return in, fmt.Errorf("no labeled papers in %s: put y or n in the relevant column", labelsPath)
	}
	in.papers, err = loadEvalPapers(ctx, filepath.Join(dir, "papers.json"), in.entries, cfg, stderr)
	return in, err
}

func cmdEval(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("eval", flag.ContinueOnError)
	fl.SetOutput(stderr)
	f := addEvalFlags(fl)
	model := fl.String("model", "", "Ollama model (default: the config's)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	cfg, err := f.load()
	if err != nil {
		return fail(stderr, err)
	}
	if *model != "" {
		cfg.Ollama.Model = *model
	}
	in, err := loadEvalInputs(ctx, *f.dir, cfg, stderr)
	if err != nil {
		return fail(stderr, err)
	}

	r, _, err := evalModel(ctx, cfg, in, *f.threshold, false, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprint(stdout, r.Text())
	path, err := writeEvalResults(filepath.Join(*f.dir, "results"), r)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "results: %s\n", path)
	return 0
}

// cmdBench runs the eval once per model and prints one comparison table.
func cmdBench(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("bench", flag.ContinueOnError)
	fl.SetOutput(stderr)
	f := addEvalFlags(fl)
	modelList := fl.String("models", defaultBenchModels, "comma-separated Ollama models to compare")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	cfg, err := f.load()
	if err != nil {
		return fail(stderr, err)
	}
	var models []string
	for _, m := range strings.Split(*modelList, ",") {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return fail(stderr, errors.New("-models is empty"))
	}

	// Check every model before running any, so a missing one fails in
	// seconds instead of after the others have run.
	var missing []string
	for _, m := range models {
		c := cfg
		c.Ollama.Model = m
		lc, err := newLLM(c)
		if err != nil {
			return fail(stderr, err)
		}
		if _, err := lc.Preflight(ctx); err != nil {
			var mm *llm.ModelMissingError
			if errors.As(err, &mm) {
				missing = append(missing, "ollama pull "+m)
				continue
			}
			return fail(stderr, err)
		}
	}
	if len(missing) > 0 {
		return fail(stderr, fmt.Errorf("these models aren't pulled yet:\n  %s", strings.Join(missing, "\n  ")))
	}

	in, err := loadEvalInputs(ctx, *f.dir, cfg, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	var rows []eval.BenchRow
	for i, m := range models {
		fmt.Fprintf(stderr, "== %s (%d/%d)\n", m, i+1, len(models))
		c := cfg
		c.Ollama.Model = m
		r, row, err := evalModel(ctx, c, in, *f.threshold, true, stderr)
		if err != nil {
			return fail(stderr, err)
		}
		path, err := writeEvalResults(filepath.Join(*f.dir, "results"), r)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "results: %s\n", path)
		rows = append(rows, row)
	}

	table := eval.BenchMarkdown(rows)
	fmt.Fprint(stdout, table)
	path, err := writeUnique(filepath.Join(*f.dir, "results"), fmt.Sprintf("%s-go-bench-%s", time.Now().Format("2006-01-02"), cfg.PromptVersion), ".md", []byte(table))
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "table: %s\n", path)
	return 0
}

// evalModel scores one model. With cold set, the model is unloaded first so
// the warm-up measures a real load (benchmarks compare load times).
func evalModel(ctx context.Context, cfg config.Config, in evalInputs, threshold int, cold bool, stderr io.Writer) (eval.Report, eval.BenchRow, error) {
	lc, err := newLLM(cfg)
	if err != nil {
		return eval.Report{}, eval.BenchRow{}, err
	}
	if _, err := lc.Preflight(ctx); err != nil {
		return eval.Report{}, eval.BenchRow{}, err
	}
	size, err := lc.ModelSize(ctx)
	if err != nil {
		return eval.Report{}, eval.BenchRow{}, err
	}
	if cold {
		if err := lc.Unload(ctx); err != nil {
			return eval.Report{}, eval.BenchRow{}, err
		}
	}
	fmt.Fprintf(stderr, "loading %s…\n", cfg.Ollama.Model)
	load, err := lc.Warmup(ctx)
	if err != nil {
		return eval.Report{}, eval.BenchRow{}, err
	}

	rows, err := eval.Evaluate(ctx, lc, in.entries, in.papers, in.labels, stderr)
	if err != nil {
		return eval.Report{}, eval.BenchRow{}, err
	}
	r := eval.Report{
		Implementation: "go",
		Date:           time.Now().Format("2006-01-02"),
		Model:          cfg.Ollama.Model,
		PromptVersion:  cfg.PromptVersion,
		Format:         cfg.Ollama.Format,
		Threshold:      threshold,
		Unlabeled:      in.unlabeled,
		Rows:           rows,
	}
	return r, eval.BenchRow{Model: cfg.Ollama.Model, SizeBytes: size, LoadMS: load.Milliseconds(), Report: r}, nil
}

// loadEvalPapers reads the local cache and fetches only what's missing, so
// every eval run scores exactly the same text.
func loadEvalPapers(ctx context.Context, path string, entries []eval.Entry, cfg config.Config, stderr io.Writer) (map[string]pubmed.Article, error) {
	papers, err := eval.LoadPapers(path)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, e := range entries {
		if _, ok := papers[e.PMID]; !ok {
			missing = append(missing, e.PMID)
		}
	}
	if len(missing) == 0 {
		return papers, nil
	}
	fmt.Fprintf(stderr, "fetching %d papers from PubMed into %s…\n", len(missing), path)
	arts, err := newPubMed(cfg).Fetch(ctx, missing)
	if err != nil {
		return nil, err
	}
	for _, a := range arts {
		papers[a.PMID] = a
	}
	return papers, eval.SavePapers(path, papers)
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// writeEvalResults writes results/<date>-<impl>-<model>-<prompt>.json.
func writeEvalResults(dir string, r eval.Report) (string, error) {
	data, err := r.JSON()
	if err != nil {
		return "", err
	}
	return writeUnique(dir, fmt.Sprintf("%s-%s-%s-%s", r.Date, r.Implementation, r.Model, r.PromptVersion), ".json", append(data, '\n'))
}

// writeUnique writes dir/<name><ext>, adding -2, -3… rather than overwriting.
func writeUnique(dir, name, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, unsafeName.ReplaceAllString(name, "_"))
	path := base
	for n := 2; ; n++ {
		_, err := os.Stat(path + ext)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		path = fmt.Sprintf("%s-%d", base, n)
	}
	return path + ext, os.WriteFile(path+ext, data, 0o644)
}
