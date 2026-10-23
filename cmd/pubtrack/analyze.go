package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pubtrack/internal/analysis"
	"pubtrack/internal/config"
	"pubtrack/internal/digest"
	"pubtrack/internal/eval"
	"pubtrack/internal/extract"
	"pubtrack/internal/factcheck"
	"pubtrack/internal/llm"
	"pubtrack/internal/pubmed"
	"pubtrack/internal/similarity"
	"pubtrack/prompts"
)

const (
	defaultJudge          = "llama3.1:8b"
	defaultEmbeddingModel = "nomic-embed-text"
	factcheckPrompt       = "factcheck-v1"
)

// stopsRun reports errors that would fail every remaining call the same way.
func stopsRun(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, llm.ErrUnavailable)
}

// preflightAll checks every model up front and lists the ones to pull.
func preflightAll(ctx context.Context, clients ...*llm.Client) error {
	var missing []string
	for _, c := range clients {
		if _, err := c.Preflight(ctx); err != nil {
			var mm *llm.ModelMissingError
			if !errors.As(err, &mm) {
				return err
			}
			missing = append(missing, "ollama pull "+mm.Model)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("these models aren't pulled yet:\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

// cmdAnalyze runs summary fact-checking, extraction, and similarity over the
// eval set and writes one report.
func cmdAnalyze(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fl.SetOutput(stderr)
	configPath := fl.String("config", "config.json", "path to the config file")
	dir := fl.String("dir", "eval", "directory with pmids.txt and labels.csv")
	model := fl.String("model", "", "model that summarizes and extracts (default: the config's)")
	prompt := fl.String("prompt", "", "summary prompt version (default: the config's)")
	judge := fl.String("judge", defaultJudge, "larger model that fact-checks the summaries")
	embedModel := fl.String("embedding-model", "", "embedding model (default: the config's, or "+defaultEmbeddingModel+")")
	force := fl.Bool("force", false, "run even though some papers aren't labeled yet")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(stderr, err)
	}
	if *model != "" {
		cfg.Ollama.Model = *model
	}
	if *prompt != "" {
		cfg.PromptVersion = *prompt
	}
	emb := *embedModel
	if emb == "" {
		emb = cfg.EmbeddingModel
	}
	if emb == "" {
		emb = defaultEmbeddingModel
	}

	entries, err := eval.ReadPMIDs(filepath.Join(*dir, "pmids.txt"))
	if err != nil {
		return fail(stderr, err)
	}
	labels, unlabeled, err := eval.ReadLabels(filepath.Join(*dir, "labels.csv"))
	if err != nil {
		return fail(stderr, err)
	}
	if unlabeled > 0 && !*force {
		return fail(stderr, fmt.Errorf("%d papers in %s aren't labeled yet. analyze shows the model's output for the eval papers, "+
			"so label them first to keep your labels independent (or pass -force)", unlabeled, filepath.Join(*dir, "labels.csv")))
	}
	xlabels, err := extract.ReadLabels(filepath.Join(*dir, "extraction_labels.csv"))
	if err != nil {
		return fail(stderr, err)
	}
	extractSystem, err := prompts.Render(cfg.ExtractPromptVersion, "")
	if err != nil {
		return fail(stderr, err)
	}
	factSystem, err := prompts.Render(factcheckPrompt, "")
	if err != nil {
		return fail(stderr, err)
	}

	lc, err := newLLM(cfg)
	if err != nil {
		return fail(stderr, err)
	}
	jcfg := cfg
	jcfg.Ollama.Model = *judge
	jc, err := newLLM(jcfg)
	if err != nil {
		return fail(stderr, err)
	}
	ec := embedClient(cfg, emb)
	if err := preflightAll(ctx, lc, jc, ec); err != nil {
		return fail(stderr, err)
	}

	// The reader's own papers are needed for similarity; cache them with the rest.
	fetch := append([]eval.Entry{}, entries...)
	for _, p := range cfg.OwnPapers {
		fetch = append(fetch, eval.Entry{PMID: p.PMID})
	}
	papers, err := loadEvalPapers(ctx, filepath.Join(*dir, "papers.json"), fetch, cfg, stderr)
	if err != nil {
		return fail(stderr, err)
	}

	var tot analysis.Totals
	rows := make([]analysis.Row, 0, len(entries))
	fmt.Fprintf(stderr, "loading %s…\n", cfg.Ollama.Model)
	if _, err := lc.Warmup(ctx); err != nil {
		return fail(stderr, err)
	}
	for i, e := range entries {
		lab, labeled := labels[e.PMID]
		row := analysis.Row{PMID: e.PMID, Pool: e.Pool, Labeled: labeled, Relevant: lab.Relevant}
		a, ok := papers[e.PMID]
		if !ok {
			row.SummaryError = "not in papers.json"
			rows = append(rows, row)
			continue
		}
		row.Title = a.Title
		p := llm.Paper{PMID: a.PMID, Title: a.Title, Journal: a.Journal, Abstract: a.Abstract}

		res, u, err := lc.Summarize(ctx, p)
		tot.Add(u)
		if stopsRun(ctx, err) {
			return fail(stderr, err)
		}
		if err != nil {
			row.SummaryError = err.Error()
		} else {
			row.Summary = res.Summary
		}

		f, u, err := extract.Run(ctx, lc, extractSystem, p)
		tot.Add(u)
		if stopsRun(ctx, err) {
			return fail(stderr, err)
		}
		if err != nil {
			row.ExtractError = err.Error()
		} else {
			row.Fields = &f
		}
		fmt.Fprintf(stderr, "[%d/%d] %s summarized and extracted\n", i+1, len(entries), e.PMID)
		rows = append(rows, row)
	}

	// Judge every summary with the larger model, after the small model is
	// done, so the two aren't swapped in and out of memory per paper.
	fmt.Fprintf(stderr, "loading judge %s…\n", *judge)
	if _, err := jc.Warmup(ctx); err != nil {
		return fail(stderr, err)
	}
	for i := range rows {
		if rows[i].Summary == "" {
			continue
		}
		v, u, err := factcheck.Check(ctx, jc, factSystem, papers[rows[i].PMID].Abstract, rows[i].Summary)
		tot.Add(u)
		if stopsRun(ctx, err) {
			return fail(stderr, err)
		}
		if err != nil {
			rows[i].JudgeError = err.Error()
		} else {
			rows[i].Verdict = &v
		}
		fmt.Fprintf(stderr, "[%d/%d] %s fact-checked\n", i+1, len(rows), rows[i].PMID)
	}

	own := embedSimilarities(ctx, ec, cfg, papers, rows, stderr)
	if ctx.Err() != nil {
		return fail(stderr, ctx.Err())
	}

	a := analysis.Analysis{
		Implementation:   "go",
		Date:             time.Now().Format("2006-01-02"),
		Model:            cfg.Ollama.Model,
		Judge:            *judge,
		EmbeddingModel:   emb,
		OwnPapers:        own,
		Rows:             rows,
		ExtractionLabels: xlabels,
		Totals:           tot,
	}
	md := a.Markdown()
	fmt.Fprint(stdout, md)
	path, err := writeUnique(filepath.Join(*dir, "results"), fmt.Sprintf("%s-go-analysis-%s", a.Date, a.Model), ".md", []byte(md))
	if err != nil {
		return fail(stderr, err)
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fail(stderr, err)
	}
	if err := os.WriteFile(strings.TrimSuffix(path, ".md")+".json", append(data, '\n'), 0o644); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "report: %s\n", path)
	return 0
}

// embedSimilarities fills in each row's similarity to the reader's papers and
// returns how many of those papers were used. A failure leaves the section
// empty with a warning; the rest of the report still stands.
func embedSimilarities(ctx context.Context, ec *llm.Client, cfg config.Config, papers map[string]pubmed.Article, rows []analysis.Row, stderr io.Writer) int {
	var inputs, labels []string
	for _, p := range cfg.OwnPapers {
		if a, ok := papers[p.PMID]; ok {
			inputs = append(inputs, similarity.Text(a.Title, a.Abstract))
			labels = append(labels, p.Label)
		}
	}
	nOwn := len(inputs)
	var idx []int
	for i, r := range rows {
		if a, ok := papers[r.PMID]; ok {
			inputs = append(inputs, similarity.Text(a.Title, a.Abstract))
			idx = append(idx, i)
		}
	}
	if nOwn == 0 || len(idx) == 0 {
		fmt.Fprintln(stderr, "similarity skipped: no papers to compare")
		return nOwn
	}
	fmt.Fprintf(stderr, "embedding %d papers with %s…\n", len(inputs), ec.Model())
	vecs, err := ec.Embed(ctx, inputs)
	if err != nil {
		fmt.Fprintf(stderr, "similarity skipped: %v\n", err)
		return nOwn
	}
	own := make([]similarity.Ref, nOwn)
	for i := range own {
		own[i] = similarity.Ref{Label: labels[i], Vec: vecs[i]}
	}
	for k, i := range idx {
		m := similarity.Best(vecs[nOwn+k], own)
		rows[i].Similarity = &m
	}
	return nOwn
}

// cmdFactcheck fact-checks the summaries in a weekly digest.
func cmdFactcheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("factcheck", flag.ContinueOnError)
	fl.SetOutput(stderr)
	configPath := fl.String("config", "config.json", "path to the config file")
	digestPath := fl.String("digest", "", "digest JSON file to check (digests/digest-YYYY-MM-DD.json)")
	judge := fl.String("judge", defaultJudge, "model that fact-checks the summaries")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *digestPath == "" {
		return fail(stderr, errors.New("-digest is required"))
	}
	data, err := os.ReadFile(*digestPath)
	if err != nil {
		return fail(stderr, err)
	}
	var d digest.Digest
	if err := json.Unmarshal(data, &d); err != nil {
		return fail(stderr, fmt.Errorf("%s: %w", *digestPath, err))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(stderr, err)
	}
	cfg.Ollama.Model = *judge
	jc, err := newLLM(cfg)
	if err != nil {
		return fail(stderr, err)
	}
	system, err := prompts.Render(factcheckPrompt, "")
	if err != nil {
		return fail(stderr, err)
	}
	if err := preflightAll(ctx, jc); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "loading judge %s…\n", *judge)
	if _, err := jc.Warmup(ctx); err != nil {
		return fail(stderr, err)
	}

	var tot analysis.Totals
	var rows []analysis.Row
	for _, it := range d.Items {
		if it.Result == nil || it.Article.Abstract == "" {
			continue
		}
		row := analysis.Row{PMID: it.Article.PMID, Title: it.Article.Title, Summary: it.Result.Summary}
		v, u, err := factcheck.Check(ctx, jc, system, it.Article.Abstract, it.Result.Summary)
		tot.Add(u)
		if stopsRun(ctx, err) {
			return fail(stderr, err)
		}
		if err != nil {
			row.JudgeError = err.Error()
		} else {
			row.Verdict = &v
		}
		fmt.Fprintf(stderr, "[%d] %s fact-checked\n", len(rows)+1, row.PMID)
		rows = append(rows, row)
	}

	title := strings.TrimSuffix(filepath.Base(*digestPath), ".json")
	report := analysis.FactcheckReport(title, *judge, rows, tot)
	fmt.Fprint(stdout, report)
	path, err := writeUnique(filepath.Dir(*digestPath), title+"-factcheck", ".md", []byte(report))
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "report: %s\n", path)
	return 0
}
