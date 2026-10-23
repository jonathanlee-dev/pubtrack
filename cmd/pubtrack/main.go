// Command pubtrack watches PubMed for new papers, has a local LLM summarize
// and score each one, and writes a markdown + JSON digest.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"pubtrack/internal/config"
	"pubtrack/internal/digest"
	"pubtrack/internal/extract"
	"pubtrack/internal/llm"
	"pubtrack/internal/ownwork"
	"pubtrack/internal/pubmed"
	"pubtrack/internal/run"
	"pubtrack/internal/state"
	"pubtrack/prompts"
)

const usage = `pubtrack watches PubMed and writes an LLM-summarized digest.

Usage:
  pubtrack check [-config config.json]             check config, Ollama, and query counts
  pubtrack run   [-config config.json] [-dry-run]  fetch, summarize, and write the digest
  pubtrack eval  [-config config.json] [-dir eval] [-model m] [-prompt v] [-format f] [-threshold 4]
                                                   score the model against your labels
  pubtrack bench [-models a,b,c] [same flags as eval, minus -model]
                                                   run eval for each model and compare them in one table
  pubtrack analyze [-dir eval] [-model m] [-judge llama3.1:8b] [-embedding-model nomic-embed-text] [-force]
                                                   fact-check summaries, score extraction, and test similarity on the eval set
  pubtrack factcheck -digest digests/digest-YYYY-MM-DD.json [-judge llama3.1:8b]
                                                   fact-check a weekly digest's summaries
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], stdout, stderr)
	case "check":
		return cmdCheck(ctx, args[1:], stdout, stderr)
	case "eval":
		return cmdEval(ctx, args[1:], stdout, stderr)
	case "bench":
		return cmdBench(ctx, args[1:], stdout, stderr)
	case "analyze":
		return cmdAnalyze(ctx, args[1:], stdout, stderr)
	case "factcheck":
		return cmdFactcheck(ctx, args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "pubtrack: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	fl.SetOutput(stderr)
	configPath := fl.String("config", "config.json", "path to the config file")
	dryRun := fl.Bool("dry-run", false, "print the digest to stdout; write no files and leave state unchanged")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(stderr, err)
	}
	st, err := state.Load(cfg.StatePath)
	if err != nil {
		return fail(stderr, err)
	}
	lc, err := newLLM(cfg)
	if err != nil {
		return fail(stderr, err)
	}

	// Check Ollama before spending any NCBI requests.
	fmt.Fprintf(stderr, "checking Ollama at %s…\n", cfg.Ollama.URL)
	if _, err := lc.Preflight(ctx); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "loading %s (slow the first time after idle)…\n", cfg.Ollama.Model)
	warm, err := lc.Warmup(ctx)
	if err != nil {
		return fail(stderr, err)
	}

	r := &run.Runner{
		Config:   cfg,
		PubMed:   pubmed.New(cfg.Tool, cfg.Email, cfg.NCBIAPIKey),
		LLM:      lc,
		State:    st,
		Matcher:  ownwork.NewMatcher(cfg.PlatformNames),
		Progress: stderr,
	}
	if cfg.ExtractFields {
		system, err := prompts.Render(cfg.ExtractPromptVersion, "")
		if err != nil {
			return fail(stderr, err)
		}
		r.Extractor = extractor{c: lc, system: system}
	}
	if cfg.EmbeddingModel != "" {
		// Similarity is a nice-to-have: a missing embedding model is a
		// warning, not a reason to skip the digest.
		ec := embedClient(cfg, cfg.EmbeddingModel)
		if _, err := ec.Preflight(ctx); err != nil {
			fmt.Fprintf(stderr, "similarity off: %v\n", err)
		} else {
			r.Embedder = ec
		}
	}
	d, err := r.Run(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	d.Stats.WarmupMillis = warm.Milliseconds()

	if *dryRun {
		fmt.Fprint(stdout, d.Markdown())
		return 0
	}
	if len(d.Items) == 0 {
		fmt.Fprintln(stderr, "no new papers; no digest written")
	} else {
		path, err := writeDigest(cfg.OutputDir, d)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "wrote %s (%d summarized, %d failed)\n", path, d.Stats.Summarized, d.Stats.Failed)
	}
	// State is saved after the digest: if the digest can't be written, the
	// same papers come back next run instead of being lost.
	if err := st.Save(cfg.StatePath); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("check", flag.ContinueOnError)
	fl.SetOutput(stderr)
	configPath := fl.String("config", "config.json", "path to the config file")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "config: ok (%d queries, %d own papers, prompt %s)\n", len(cfg.Queries), len(cfg.OwnPapers), cfg.PromptVersion)
	if cfg.Email == "you@example.com" {
		fmt.Fprintln(stdout, "config: warning: email is still the example address; NCBI asks for a real contact")
	}

	code := 0
	lc, err := newLLM(cfg)
	if err != nil {
		return fail(stderr, err)
	}
	if v, err := lc.Preflight(ctx); err != nil {
		fmt.Fprintf(stdout, "ollama: FAIL: %v\n", err)
		code = 1
	} else {
		fmt.Fprintf(stdout, "ollama: ok (version %s, model %s)\n", v, cfg.Ollama.Model)
	}

	pm := pubmed.New(cfg.Tool, cfg.Email, cfg.NCBIAPIKey)
	fmt.Fprintf(stdout, "pubmed: papers added in the last %d days\n", cfg.WindowDays)
	for _, q := range cfg.Queries {
		res, err := pm.Search(ctx, q.Term, cfg.WindowDays)
		if err != nil {
			fmt.Fprintf(stdout, "  %-28s FAIL: %v\n", q.Name, err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "  %-28s %5d\n", q.Name, res.Count)
		if len(res.PhrasesNotFound) > 0 {
			fmt.Fprintf(stdout, "  %-28s warning: PubMed ignored %v\n", "", res.PhrasesNotFound)
		}
	}
	return code
}

// extractor binds the extraction prompt to a client for the runner.
type extractor struct {
	c      *llm.Client
	system string
}

func (e extractor) Extract(ctx context.Context, p llm.Paper) (extract.Fields, llm.Usage, error) {
	return extract.Run(ctx, e.c, e.system, p)
}

// embedClient is a client for an embedding model; it needs no prompt.
func embedClient(cfg config.Config, model string) *llm.Client {
	return llm.New(llm.Options{
		BaseURL:        cfg.Ollama.URL,
		Model:          model,
		RequestTimeout: time.Duration(cfg.Ollama.RequestTimeoutSeconds) * time.Second,
		LoadTimeout:    time.Duration(cfg.Ollama.LoadTimeoutSeconds) * time.Second,
	})
}

func newLLM(cfg config.Config) (*llm.Client, error) {
	system, err := prompts.Render(cfg.PromptVersion, cfg.Profile)
	if err != nil {
		return nil, err
	}
	return llm.New(llm.Options{
		BaseURL:          cfg.Ollama.URL,
		Model:            cfg.Ollama.Model,
		SystemPrompt:     system,
		Format:           cfg.Ollama.Format,
		NumCtx:           cfg.Ollama.NumCtx,
		NumPredict:       cfg.Ollama.NumPredict,
		MaxAbstractChars: cfg.MaxAbstractChars,
		RequestTimeout:   time.Duration(cfg.Ollama.RequestTimeoutSeconds) * time.Second,
		LoadTimeout:      time.Duration(cfg.Ollama.LoadTimeoutSeconds) * time.Second,
	}), nil
}

// writeDigest writes digest-<date>.md and .json, adding -2, -3… rather than
// overwriting an earlier digest from the same day.
func writeDigest(dir string, d digest.Digest) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, "digest-"+d.Date)
	name := base
	for n := 2; ; n++ {
		_, err := os.Stat(name + ".md")
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	js, err := d.JSON()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(name+".json", js, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(name+".md", []byte(d.Markdown()), 0o644); err != nil {
		return "", err
	}
	return name + ".md", nil
}

func fail(w io.Writer, err error) int {
	fmt.Fprintf(w, "pubtrack: %v\n", err)
	return 1
}
