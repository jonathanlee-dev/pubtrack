// Package run ties one pubtrack run together: search, dedupe, fetch, flag,
// summarize, and collect the digest. The network clients are interfaces so
// the whole flow is testable without NCBI or Ollama.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"pubtrack/internal/config"
	"pubtrack/internal/digest"
	"pubtrack/internal/extract"
	"pubtrack/internal/llm"
	"pubtrack/internal/ownwork"
	"pubtrack/internal/pubmed"
	"pubtrack/internal/similarity"
	"pubtrack/internal/state"
)

type PubMed interface {
	Search(ctx context.Context, term string, days int) (pubmed.SearchResult, error)
	CitedBy(ctx context.Context, pmids []string) (map[string][]string, error)
	Fetch(ctx context.Context, pmids []string) ([]pubmed.Article, error)
}

type Summarizer interface {
	Summarize(ctx context.Context, p llm.Paper) (llm.Result, llm.Usage, error)
}

type Extractor interface {
	Extract(ctx context.Context, p llm.Paper) (extract.Fields, llm.Usage, error)
}

type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float64, error)
}

type Runner struct {
	Config  config.Config
	PubMed  PubMed
	LLM     Summarizer
	State   *state.State
	Matcher ownwork.Matcher
	// Extractor and Embedder are optional; nil turns the feature off.
	Extractor Extractor
	Embedder  Embedder
	Now       func() time.Time
	// Progress, if set, gets one line per paper as it's processed.
	Progress io.Writer
}

// Run updates r.State in memory; the caller saves it after writing the digest.
func (r *Runner) Run(ctx context.Context) (digest.Digest, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	today := now().Format("2006-01-02")
	d := digest.Digest{
		Date:          today,
		WindowDays:    r.Config.WindowDays,
		Model:         r.Config.Ollama.Model,
		PromptVersion: r.Config.PromptVersion,
	}

	topics, err := r.search(ctx, &d)
	if err != nil {
		return d, err
	}
	cites, err := r.citations(ctx, &d, topics)
	if err != nil {
		return d, err
	}
	ids := r.candidates(topics, cites)
	d.Stats.Candidates = len(ids)
	if len(ids) == 0 {
		return d, nil
	}

	// Similarity needs the reader's own papers too; they ride along in the
	// same efetch call and are never candidates themselves.
	fetchIDs := ids
	if r.Embedder != nil {
		for _, p := range r.Config.OwnPapers {
			fetchIDs = append(fetchIDs[:len(fetchIDs):len(fetchIDs)], p.PMID)
		}
	}
	articles, err := r.PubMed.Fetch(ctx, fetchIDs)
	if err != nil {
		return d, fmt.Errorf("fetching %d papers from PubMed: %w", len(ids), err)
	}
	byID := make(map[string]pubmed.Article, len(articles))
	for _, a := range articles {
		byID[a.PMID] = a
	}
	dups := r.duplicates(ids, byID)
	fresh := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dup := dups[id]; !dup {
			fresh = append(fresh, id)
		}
	}
	sims, err := r.similarities(ctx, &d, fresh, byID)
	if err != nil {
		return d, err
	}

	unavailable := false
	for i, id := range ids {
		item := digest.Item{Article: pubmed.Article{PMID: id}, Topics: topics[id], Cites: cites[id]}
		a, ok := byID[id]
		switch {
		case !ok:
			item.Error = "not returned by PubMed efetch"
			r.fail(&d, &item, today)

		case a.IsErratum():
			r.State.MarkSeen(id, today)
			d.Stats.SkippedErrata++
			r.progress(i, len(ids), id, "skipped (erratum)")
			continue

		case dups[id] != "":
			r.State.MarkSeen(id, "duplicate of "+dups[id])
			d.Stats.Duplicates++
			r.progress(i, len(ids), id, "skipped (duplicate of "+dups[id]+")")
			continue

		default:
			item.Article = a
			item.Mentions = r.Matcher.Mentions(a.Title + "\n" + a.Abstract)
			if m, ok := sims[id]; ok {
				item.Similarity = &m
				d.Stats.Similarities++
			}
			if strings.TrimSpace(a.Abstract) == "" {
				// Scoring a title alone invites the model to make things up.
				item.Note = "No abstract in PubMed, so not scored."
				r.State.MarkSeen(id, today)
				r.State.RememberKeys(id, a.DedupeKeys())
				d.Stats.NoAbstract++
				break
			}
			if unavailable {
				item.Error = "not summarized: Ollama stopped responding earlier in this run"
				r.State.KeepPending(id)
				d.Stats.Failed++
				break
			}

			paper := llm.Paper{PMID: id, Title: a.Title, Journal: a.Journal, Abstract: a.Abstract}
			res, usage, err := r.LLM.Summarize(ctx, paper)
			d.Stats.AddUsage(usage)
			item.Usage = &usage
			switch {
			case err == nil:
				item.Result = &res
				r.State.MarkSeen(id, today)
				r.State.RememberKeys(id, a.DedupeKeys())
				d.Stats.Summarized++
				gone, err := r.extract(ctx, &d, &item, paper)
				if err != nil {
					return d, err
				}
				if gone {
					unavailable = true
					d.Warnings = append(d.Warnings, "Ollama stopped responding; the remaining papers will be retried next run")
				}
			case ctx.Err() != nil:
				return d, ctx.Err()
			case errors.Is(err, llm.ErrUnavailable):
				unavailable = true
				item.Error = "not summarized: " + err.Error()
				r.State.KeepPending(id)
				d.Stats.Failed++
				d.Warnings = append(d.Warnings, "Ollama stopped responding; the remaining papers will be retried next run")
			default:
				item.Error = err.Error()
				r.fail(&d, &item, today)
			}
		}
		r.progress(i, len(ids), id, status(item))
		d.Items = append(d.Items, item)
	}
	return d, nil
}

// duplicates maps each candidate that is the same paper as another, seen in
// an earlier run or earlier in this one, to that paper's PMID. Within a run
// the first candidate in processing order is kept.
func (r *Runner) duplicates(ids []string, byID map[string]pubmed.Article) map[string]string {
	dups := map[string]string{}
	inRun := map[string]string{} // key → PMID kept in this run
	for _, id := range ids {
		a, ok := byID[id]
		if !ok || a.IsErratum() {
			continue
		}
		keys := a.DedupeKeys()
		if first, ok := r.State.DuplicateOf(id, keys); ok {
			dups[id] = first
			continue
		}
		dup := false
		for _, k := range keys {
			if first, ok := inRun[k]; ok {
				dups[id] = first
				dup = true
				break
			}
		}
		if !dup {
			for _, k := range keys {
				inRun[k] = id
			}
		}
	}
	return dups
}

// extract adds structured fields to a summarized paper. A failure is noted on
// the item, not counted against the paper: its summary already succeeded.
// It reports whether Ollama has gone away.
func (r *Runner) extract(ctx context.Context, d *digest.Digest, item *digest.Item, p llm.Paper) (bool, error) {
	if r.Extractor == nil {
		return false, nil
	}
	f, u, err := r.Extractor.Extract(ctx, p)
	d.Stats.AddExtraUsage(u)
	switch {
	case err == nil:
		item.Extraction = &f
		d.Stats.Extracted++
	case ctx.Err() != nil:
		return false, ctx.Err()
	default:
		item.ExtractError = err.Error()
		d.Stats.ExtractFailed++
	}
	return errors.Is(err, llm.ErrUnavailable), nil
}

// similarities embeds the reader's papers and every candidate in one request
// and returns each candidate's closest match. Any problem is a warning: the
// digest is still useful without similarity.
func (r *Runner) similarities(ctx context.Context, d *digest.Digest, ids []string, byID map[string]pubmed.Article) (map[string]similarity.Match, error) {
	if r.Embedder == nil {
		return nil, nil
	}
	var inputs, labels []string
	for _, p := range r.Config.OwnPapers {
		if a, ok := byID[p.PMID]; ok {
			inputs = append(inputs, similarity.Text(a.Title, a.Abstract))
			labels = append(labels, p.Label)
		}
	}
	if len(inputs) == 0 {
		d.Warnings = append(d.Warnings, "similarity skipped: none of your own papers came back from PubMed")
		return nil, nil
	}
	nOwn := len(inputs)
	var candidates []string
	for _, id := range ids {
		if a, ok := byID[id]; ok && !a.IsErratum() {
			inputs = append(inputs, similarity.Text(a.Title, a.Abstract))
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	vecs, err := r.Embedder.Embed(ctx, inputs)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d.Warnings = append(d.Warnings, fmt.Sprintf("similarity skipped: %v", err))
		return nil, nil
	}
	own := make([]similarity.Ref, nOwn)
	for i := range own {
		own[i] = similarity.Ref{Label: labels[i], Vec: vecs[i]}
	}
	out := make(map[string]similarity.Match, len(candidates))
	for i, id := range candidates {
		out[id] = similarity.Best(vecs[nOwn+i], own)
	}
	return out, nil
}

// search runs every query and returns PMID → names of the queries that
// matched it. One failing query is a warning; all of them failing is an error.
func (r *Runner) search(ctx context.Context, d *digest.Digest) (map[string][]string, error) {
	topics := map[string][]string{}
	failed := 0
	var firstErr error
	for _, q := range r.Config.Queries {
		res, err := r.PubMed.Search(ctx, q.Term, r.Config.WindowDays)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failed++
			if firstErr == nil {
				firstErr = err
			}
			d.Warnings = append(d.Warnings, fmt.Sprintf("query %q failed: %v", q.Name, err))
			continue
		}
		if len(res.PhrasesNotFound) > 0 {
			d.Warnings = append(d.Warnings, fmt.Sprintf("query %q: PubMed ignored %v (typo?)", q.Name, res.PhrasesNotFound))
		}
		if res.Count > len(res.IDs) {
			d.Warnings = append(d.Warnings, fmt.Sprintf("query %q matched %d papers; only the first %d were used", q.Name, res.Count, len(res.IDs)))
		}
		for _, id := range res.IDs {
			topics[id] = append(topics[id], q.Name)
		}
	}
	if failed > 0 && failed == len(r.Config.Queries) {
		return nil, fmt.Errorf("every query failed; first error: %w", firstErr)
	}
	return topics, nil
}

// citations returns citing PMID → labels of the reader's papers it cites.
func (r *Runner) citations(ctx context.Context, d *digest.Digest, topics map[string][]string) (map[string][]string, error) {
	cites := map[string][]string{}
	if len(r.Config.OwnPapers) == 0 {
		return cites, nil
	}
	labels := map[string]string{}
	own := make([]string, 0, len(r.Config.OwnPapers))
	for _, p := range r.Config.OwnPapers {
		labels[p.PMID] = p.Label
		own = append(own, p.PMID)
	}
	citing, err := r.PubMed.CitedBy(ctx, own)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d.Warnings = append(d.Warnings, fmt.Sprintf("citation lookup failed: %v", err))
		return cites, nil
	}
	for c, cited := range citing {
		for _, o := range cited {
			cites[c] = append(cites[c], labels[o])
		}
	}

	// The citation link has no date filter, so on the first run every paper
	// that ever cited the reader's work would show up. Record those as seen,
	// unless a query matched them too (then they're in the window anyway).
	if !r.State.CitationsBaselined {
		for c := range cites {
			if _, inWindow := topics[c]; !inWindow && !r.State.IsSeen(c) {
				r.State.MarkSeen(c, "baseline")
				d.Stats.Baselined++
			}
		}
		r.State.CitationsBaselined = true
	}
	return cites, nil
}

func (r *Runner) candidates(topics, cites map[string][]string) []string {
	own := map[string]bool{}
	for _, p := range r.Config.OwnPapers {
		own[p.PMID] = true
	}
	set := map[string]bool{}
	for id := range topics {
		set[id] = true
	}
	for id := range cites {
		set[id] = true
	}
	for id := range r.State.Failed {
		set[id] = true
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		if !own[id] && !r.State.IsSeen(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// fail records a failed attempt that was about the paper itself, and gives
// up after MaxAttempts so one bad record can't be retried forever.
func (r *Runner) fail(d *digest.Digest, item *digest.Item, today string) {
	d.Stats.Failed++
	id := item.Article.PMID
	n := r.State.MarkFailed(id)
	limit := r.Config.MaxAttempts
	if n >= limit {
		r.State.MarkSeen(id, "gave-up "+today)
		item.Error += fmt.Sprintf(" (attempt %d of %d; giving up)", n, limit)
		return
	}
	item.Error += fmt.Sprintf(" (attempt %d of %d; will retry next run)", n, limit)
}

func (r *Runner) progress(i, n int, id, msg string) {
	if r.Progress != nil {
		fmt.Fprintf(r.Progress, "[%d/%d] %s %s\n", i+1, n, id, msg)
	}
}

func status(it digest.Item) string {
	switch {
	case it.Error != "":
		return "failed: " + it.Error
	case it.Result != nil:
		return fmt.Sprintf("relevance %d (%.1fs)", it.Result.Relevance, float64(it.Usage.LatencyMS)/1000)
	default:
		return "no abstract"
	}
}
