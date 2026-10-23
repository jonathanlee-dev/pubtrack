package run

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"pubtrack/internal/config"
	"pubtrack/internal/digest"
	"pubtrack/internal/extract"
	"pubtrack/internal/llm"
	"pubtrack/internal/ownwork"
	"pubtrack/internal/pubmed"
	"pubtrack/internal/state"
)

type fakePubMed struct {
	results   map[string][]string // term → PMIDs
	searchErr error
	citedBy   map[string][]string
	articles  map[string]pubmed.Article
	fetched   []string
}

func (f *fakePubMed) Search(_ context.Context, term string, _ int) (pubmed.SearchResult, error) {
	if f.searchErr != nil {
		return pubmed.SearchResult{}, f.searchErr
	}
	ids := f.results[term]
	return pubmed.SearchResult{Count: len(ids), IDs: ids}, nil
}

func (f *fakePubMed) CitedBy(context.Context, []string) (map[string][]string, error) {
	return f.citedBy, nil
}

func (f *fakePubMed) Fetch(_ context.Context, ids []string) ([]pubmed.Article, error) {
	f.fetched = append(f.fetched, ids...)
	var out []pubmed.Article
	for _, id := range ids {
		if a, ok := f.articles[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

type fakeLLM struct {
	errs  map[string]error
	calls []string
}

func (f *fakeLLM) Summarize(_ context.Context, p llm.Paper) (llm.Result, llm.Usage, error) {
	f.calls = append(f.calls, p.PMID)
	u := llm.Usage{PromptTokens: 100, OutputTokens: 20, LatencyMS: 1000, Attempts: 1}
	if err := f.errs[p.PMID]; err != nil {
		return llm.Result{}, u, err
	}
	return llm.Result{Summary: "S.", Relevance: 4, Reason: "R."}, u, nil
}

func articles(ids ...string) map[string]pubmed.Article {
	m := map[string]pubmed.Article{}
	for _, id := range ids {
		m[id] = pubmed.Article{PMID: id, Title: "Title " + id, Abstract: "Abstract " + id}
	}
	return m
}

func newRunner(pm *fakePubMed, l *fakeLLM, st *state.State) *Runner {
	return &Runner{
		Config: config.Config{
			WindowDays:  10,
			MaxAttempts: 3,
			Queries:     []config.Query{{Name: "q1", Term: "t1"}},
			OwnPapers:   []config.OwnPaper{{PMID: "1", Label: "Own 1"}},
		},
		PubMed:  pm,
		LLM:     l,
		State:   st,
		Matcher: ownwork.NewMatcher([]string{"ORCATECH"}),
		Now:     func() time.Time { return time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC) },
	}
}

// baselined returns state as it is after the first run.
func baselined() *state.State {
	st := state.New()
	st.CitationsBaselined = true
	return st
}

func item(t *testing.T, d digest.Digest, pmid string) digest.Item {
	t.Helper()
	for _, it := range d.Items {
		if it.Article.PMID == pmid {
			return it
		}
	}
	t.Fatalf("PMID %s not in digest", pmid)
	return digest.Item{}
}

func TestOnePaperFailingDoesNotAbortRun(t *testing.T) {
	pm := &fakePubMed{results: map[string][]string{"t1": {"10", "11", "12"}}, articles: articles("10", "11", "12")}
	l := &fakeLLM{errs: map[string]error{"11": &llm.ValidationError{Problem: "missing \"reason\""}}}
	st := baselined()
	d, err := newRunner(pm, l, st).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Items) != 3 || d.Stats.Summarized != 2 || d.Stats.Failed != 1 {
		t.Fatalf("items=%d summarized=%d failed=%d", len(d.Items), d.Stats.Summarized, d.Stats.Failed)
	}
	if e := item(t, d, "11").Error; !strings.Contains(e, "attempt 1 of 3; will retry next run") {
		t.Errorf("error = %q", e)
	}
	if !st.IsSeen("10") || !st.IsSeen("12") || st.IsSeen("11") || st.Failed["11"] != 1 {
		t.Errorf("state = %+v", st)
	}
	if d.Stats.PromptTokens != 300 || d.Stats.LLMPapers != 3 {
		t.Errorf("usage not counted for every call: %+v", d.Stats)
	}
}

func TestSeenPapersAreSkipped(t *testing.T) {
	pm := &fakePubMed{results: map[string][]string{"t1": {"10", "11"}}, articles: articles("10", "11")}
	l := &fakeLLM{}
	st := baselined()
	st.MarkSeen("10", "2026-10-12")
	if _, err := newRunner(pm, l, st).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pm.fetched, []string{"11"}) || !reflect.DeepEqual(l.calls, []string{"11"}) {
		t.Errorf("fetched %v, summarized %v; want only 11", pm.fetched, l.calls)
	}
}

func TestFailedPaperIsRetriedThenGivenUp(t *testing.T) {
	pm := &fakePubMed{results: map[string][]string{}, articles: articles("20")}
	l := &fakeLLM{errs: map[string]error{"20": &llm.ValidationError{Problem: "x"}}}
	st := baselined()
	st.Failed["20"] = 2 // out of the search window now, but still owed a retry
	d, err := newRunner(pm, l, st).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.calls, []string{"20"}) {
		t.Fatalf("summarized %v, want [20]", l.calls)
	}
	if e := item(t, d, "20").Error; !strings.Contains(e, "attempt 3 of 3; giving up") {
		t.Errorf("error = %q", e)
	}
	if _, still := st.Failed["20"]; still || !strings.HasPrefix(st.Seen["20"], "gave-up") {
		t.Errorf("state = %+v", st)
	}
}

func TestFirstRunBaselinesExistingCitations(t *testing.T) {
	pm := &fakePubMed{
		results:  map[string][]string{"t1": {"30"}},
		citedBy:  map[string][]string{"30": {"1"}, "31": {"1"}},
		articles: articles("30", "31", "32"),
	}
	st := state.New()
	d, err := newRunner(pm, &fakeLLM{}, st).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 31 already cited the reader's work before the first run: history, not news.
	// 30 also matched a query, so it's in the window and gets processed.
	if st.Seen["31"] != "baseline" || d.Stats.Baselined != 1 || !st.CitationsBaselined {
		t.Errorf("baseline: seen=%v baselined=%d flag=%v", st.Seen, d.Stats.Baselined, st.CitationsBaselined)
	}
	if got := item(t, d, "30").Cites; !reflect.DeepEqual(got, []string{"Own 1"}) {
		t.Errorf("30 cites %v", got)
	}

	// Next run: a newly linked citing paper is news, whether or not a query matched.
	pm.citedBy["32"] = []string{"1"}
	pm.fetched = nil
	d, err = newRunner(pm, &fakeLLM{}, st).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pm.fetched, []string{"32"}) {
		t.Errorf("second run fetched %v, want [32]", pm.fetched)
	}
	if got := item(t, d, "32").Cites; !reflect.DeepEqual(got, []string{"Own 1"}) {
		t.Errorf("32 cites %v", got)
	}
}

func TestOllamaGoingAwayStopsFurtherCalls(t *testing.T) {
	pm := &fakePubMed{results: map[string][]string{"t1": {"40", "41", "42"}}, articles: articles("40", "41", "42")}
	l := &fakeLLM{errs: map[string]error{"40": fmt.Errorf("dial: %w", llm.ErrUnavailable)}}
	st := baselined()
	d, err := newRunner(pm, l, st).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.calls, []string{"40"}) {
		t.Errorf("summarize calls = %v, want only the first", l.calls)
	}
	for _, id := range []string{"40", "41", "42"} {
		if item(t, d, id).Error == "" {
			t.Errorf("%s has no error", id)
		}
		// Not the paper's fault: queued for next run without using an attempt.
		if n, ok := st.Failed[id]; !ok || n != 0 || st.IsSeen(id) {
			t.Errorf("%s: failed=%d,%v seen=%v", id, n, ok, st.IsSeen(id))
		}
	}
	if len(d.Warnings) != 1 {
		t.Errorf("warnings = %v", d.Warnings)
	}
}

func TestNoAbstractErratumAndMissingPMID(t *testing.T) {
	arts := map[string]pubmed.Article{
		"50": {PMID: "50", Title: "Letter"},
		"51": {PMID: "51", Title: "Correction", PublicationTypes: []string{"Published Erratum"}},
	}
	pm := &fakePubMed{results: map[string][]string{"t1": {"50", "51", "52"}}, articles: arts}
	l := &fakeLLM{}
	st := baselined()
	d, err := newRunner(pm, l, st).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.calls) != 0 {
		t.Errorf("model called for %v; nothing here has an abstract", l.calls)
	}
	if it := item(t, d, "50"); it.Note == "" || it.Result != nil {
		t.Errorf("no-abstract item = %+v", it)
	}
	for _, it := range d.Items {
		if it.Article.PMID == "51" {
			t.Error("erratum should be skipped, not listed")
		}
	}
	if !strings.Contains(item(t, d, "52").Error, "not returned by PubMed") {
		t.Error("missing PMID not reported")
	}
	if !st.IsSeen("50") || !st.IsSeen("51") || st.Failed["52"] != 1 {
		t.Errorf("state = %+v", st)
	}
	if d.Stats.NoAbstract != 1 || d.Stats.SkippedErrata != 1 || d.Stats.Failed != 1 {
		t.Errorf("stats = %+v", d.Stats)
	}
}

func TestOwnPapersAreNeverCandidates(t *testing.T) {
	pm := &fakePubMed{results: map[string][]string{"t1": {"1", "60"}}, articles: articles("1", "60")}
	if _, err := newRunner(pm, &fakeLLM{}, baselined()).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pm.fetched, []string{"60"}) {
		t.Errorf("fetched %v, want [60]", pm.fetched)
	}
}

func TestPlatformMentionsFlagged(t *testing.T) {
	arts := map[string]pubmed.Article{"70": {PMID: "70", Title: "T", Abstract: "Data from the ORCATECH cohort."}}
	pm := &fakePubMed{results: map[string][]string{"t1": {"70"}}, articles: arts}
	d, err := newRunner(pm, &fakeLLM{}, baselined()).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := item(t, d, "70").Mentions; !reflect.DeepEqual(got, []string{"ORCATECH"}) {
		t.Errorf("mentions = %v", got)
	}
}

func TestEveryQueryFailingIsAnError(t *testing.T) {
	pm := &fakePubMed{searchErr: errors.New("esearch: HTTP 503")}
	if _, err := newRunner(pm, &fakeLLM{}, baselined()).Run(context.Background()); err == nil {
		t.Error("expected an error when no query succeeds")
	}
}

type fakeExtractor struct {
	errs  map[string]error
	calls []string
}

func (f *fakeExtractor) Extract(_ context.Context, p llm.Paper) (extract.Fields, llm.Usage, error) {
	f.calls = append(f.calls, p.PMID)
	u := llm.Usage{PromptTokens: 50, OutputTokens: 10, LatencyMS: 500, Attempts: 1}
	if err := f.errs[p.PMID]; err != nil {
		return extract.Fields{}, u, err
	}
	return extract.Fields{StudyDesign: "observational", SampleSize: 10, Setting: "home", Population: "p", Technology: []string{}}, u, nil
}

// fakeEmbedder puts each text on an axis by keyword, so similarity is predictable.
type fakeEmbedder struct {
	inputs []string
	err    error
}

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float64, error) {
	f.inputs = inputs
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float64, len(inputs))
	for i, in := range inputs {
		switch {
		case strings.Contains(in, "gait"):
			out[i] = []float64{1, 0}
		case strings.Contains(in, "sleep"):
			out[i] = []float64{0, 1}
		default:
			out[i] = []float64{1, 1}
		}
	}
	return out, nil
}

func TestExtractionRunsAfterSummaries(t *testing.T) {
	pm := &fakePubMed{results: map[string][]string{"t1": {"10", "11"}}, articles: articles("10", "11")}
	l := &fakeLLM{errs: map[string]error{"11": &llm.ValidationError{Problem: "x"}}}
	ex := &fakeExtractor{}
	r := newRunner(pm, l, baselined())
	r.Extractor = ex
	d, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ex.calls, []string{"10"}) {
		t.Errorf("extracted %v; only papers whose summary succeeded should be", ex.calls)
	}
	if item(t, d, "10").Extraction == nil || d.Stats.Extracted != 1 {
		t.Errorf("extraction missing: %+v", d.Stats)
	}
	// Extraction tokens count toward the run, but not as another paper.
	if d.Stats.PromptTokens != 250 || d.Stats.LLMPapers != 2 {
		t.Errorf("stats = %+v", d.Stats)
	}
}

func TestExtractionFailureDoesNotFailThePaper(t *testing.T) {
	pm := &fakePubMed{results: map[string][]string{"t1": {"10"}}, articles: articles("10")}
	r := newRunner(pm, &fakeLLM{}, baselined())
	st := r.State
	r.Extractor = &fakeExtractor{errs: map[string]error{"10": &llm.ValidationError{Problem: "x"}}}
	d, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	it := item(t, d, "10")
	if it.Result == nil || it.Error != "" || it.ExtractError == "" || !st.IsSeen("10") {
		t.Errorf("item = %+v", it)
	}
	if d.Stats.ExtractFailed != 1 {
		t.Errorf("stats = %+v", d.Stats)
	}
}

func TestSimilarityToOwnPapers(t *testing.T) {
	arts := map[string]pubmed.Article{
		"1":  {PMID: "1", Title: "Own gait paper", Abstract: "gait"},
		"2":  {PMID: "2", Title: "Own sleep paper", Abstract: "sleep"},
		"10": {PMID: "10", Title: "New", Abstract: "sleep sensors"},
		"11": {PMID: "11", Title: "Correction", PublicationTypes: []string{"Published Erratum"}},
	}
	pm := &fakePubMed{results: map[string][]string{"t1": {"10", "11"}}, articles: arts}
	emb := &fakeEmbedder{}
	r := newRunner(pm, &fakeLLM{}, baselined())
	r.Config.OwnPapers = []config.OwnPaper{{PMID: "1", Label: "Gait"}, {PMID: "2", Label: "Sleep"}}
	r.Embedder = emb
	d, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pm.fetched, []string{"10", "11", "1", "2"}) {
		t.Errorf("fetched %v; own papers should ride along in the same request", pm.fetched)
	}
	if len(emb.inputs) != 3 || !strings.HasPrefix(emb.inputs[0], "clustering: Own gait paper") {
		t.Errorf("embedded %q; errata shouldn't be embedded", emb.inputs)
	}
	sim := item(t, d, "10").Similarity
	if sim == nil || sim.Closest != "Sleep" || sim.Score != 1 {
		t.Errorf("similarity = %+v", sim)
	}
	for _, it := range d.Items {
		if it.Article.PMID == "1" || it.Article.PMID == "2" {
			t.Error("own papers must not appear in the digest")
		}
	}
}

func TestSimilarityFailureIsAWarning(t *testing.T) {
	arts := articles("1", "10")
	pm := &fakePubMed{results: map[string][]string{"t1": {"10"}}, articles: arts}
	r := newRunner(pm, &fakeLLM{}, baselined())
	r.Embedder = &fakeEmbedder{err: errors.New("model not found")}
	d, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item(t, d, "10").Similarity != nil || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "similarity skipped") {
		t.Errorf("warnings = %v", d.Warnings)
	}
}
