package eval

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pubtrack/internal/llm"
	"pubtrack/internal/pubmed"
)

var update = flag.Bool("update", false, "rewrite golden files")

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadPMIDs(t *testing.T) {
	p := write(t, "pmids.txt", "# comment\n\n42866529\tin_home_sensing\n  41278633\town_platforms  \n42895279\n")
	got, err := ReadPMIDs(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{"42866529", "in_home_sensing"}, {"41278633", "own_platforms"}, {"42895279", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if _, err := ReadPMIDs(write(t, "bad.txt", "PMC123\tx\n")); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Errorf("err = %v, want a line-numbered error", err)
	}
}

func TestReadLabels(t *testing.T) {
	// A BOM and CRLF line endings, as a spreadsheet app might save it, and a
	// quoted title containing a comma and a quote.
	csv := "\ufeffn,pmid,relevant,note,title\r\n" +
		"1,100,y,,\"Gait, sensors, and \"\"aging\"\"\"\r\n" +
		"2,200,N,hard call,Second\r\n" +
		"3,300,,,Not labeled yet\r\n" +
		"4,400,Yes,,Fourth\r\n"
	labels, unlabeled, err := ReadLabels(write(t, "labels.csv", csv))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Label{"100": {Relevant: true}, "200": {Relevant: false, Note: "hard call"}, "400": {Relevant: true}}
	if !reflect.DeepEqual(labels, want) || unlabeled != 1 {
		t.Errorf("got %v (unlabeled %d), want %v (unlabeled 1)", labels, unlabeled, want)
	}

	_, _, err = ReadLabels(write(t, "bad.csv", "n,pmid,relevant\n1,100,maybe\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "maybe") {
		t.Errorf("err = %v, want it to name line 2 and the bad value", err)
	}
}

func TestRatioRoundsHalfUp(t *testing.T) {
	for _, tt := range []struct {
		num, den int
		want     string
	}{{10, 13, "0.77"}, {1, 8, "0.13"}, {2, 3, "0.67"}, {5, 5, "1.00"}, {0, 4, "0.00"}, {0, 0, "n/a"}} {
		if got := Ratio(tt.num, tt.den); got != tt.want {
			t.Errorf("Ratio(%d, %d) = %q, want %q", tt.num, tt.den, got, tt.want)
		}
	}
}

func sampleRows() []Row {
	u := llm.Usage{PromptTokens: 800, OutputTokens: 100, LatencyMS: 2000, Attempts: 1}
	return []Row{
		{PMID: "105", Pool: "in_home_sensing", Title: "True positive", Relevant: true, Score: 5, Reason: "R.", Usage: u},
		{PMID: "104", Pool: "digital_biomarkers", Title: "Missed paper", Relevant: true, Score: 3, Reason: "Only adjacent.", Note: "hard call", Usage: u},
		{PMID: "103", Pool: "negative_cart", Title: "False alarm", Relevant: false, Score: 4, Reason: "Mentions sensors.", Usage: u},
		{PMID: "102", Pool: "negative_cart", Title: "True negative", Relevant: false, Score: 1, Reason: "R.", Usage: u},
		{PMID: "101", Pool: "own_platforms", Title: "Broken reply", Relevant: true, Error: "invalid model output: missing \"reason\"", Usage: llm.Usage{PromptTokens: 900, OutputTokens: 50, LatencyMS: 2100, Attempts: 2}},
	}
}

func TestCompute(t *testing.T) {
	m := Compute(sampleRows(), 4)
	if m != (Metrics{TP: 1, FP: 1, FN: 1, TN: 1, Failed: 1}) {
		t.Errorf("metrics = %+v", m)
	}
}

// The report is pinned by a golden file.
func TestReportGolden(t *testing.T) {
	r := Report{Model: "llama3.2:3b", PromptVersion: "v1", Format: "schema", Threshold: 4, Unlabeled: 2, Rows: sampleRows()}
	got := r.Text()
	path := filepath.Join("testdata", "report.golden.txt")
	want, err := os.ReadFile(path)
	if *update || os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s; review it before committing", path)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("report differs from %s (run with -update if intended):\n%s", path, got)
	}
}

type fakeSummarizer struct {
	errs  map[string]error
	calls []string
}

func (f *fakeSummarizer) Summarize(_ context.Context, p llm.Paper) (llm.Result, llm.Usage, error) {
	f.calls = append(f.calls, p.PMID)
	u := llm.Usage{Attempts: 1}
	if err := f.errs[p.PMID]; err != nil {
		return llm.Result{}, u, err
	}
	return llm.Result{Summary: "S.", Relevance: 4, Reason: "R."}, u, nil
}

func TestEvaluate(t *testing.T) {
	entries := []Entry{{"1", "a"}, {"2", "b"}, {"3", "c"}, {"4", "d"}}
	papers := map[string]pubmed.Article{
		"1": {PMID: "1", Title: "One", Abstract: "A"},
		"2": {PMID: "2", Title: "Two", Abstract: "A"},
		"3": {PMID: "3", Title: "Three", Abstract: "A"},
	}
	labels := map[string]Label{"1": {Relevant: true}, "2": {Relevant: false}, "4": {Relevant: true}} // 3 unlabeled
	s := &fakeSummarizer{errs: map[string]error{"2": &llm.ValidationError{Problem: "x"}}}

	rows, err := Evaluate(context.Background(), s, entries, papers, labels, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.calls, []string{"1", "2"}) {
		t.Errorf("summarized %v; unlabeled and missing papers shouldn't be sent", s.calls)
	}
	if len(rows) != 3 || rows[0].Score != 4 || rows[1].Error == "" || !strings.Contains(rows[2].Error, "papers.json") {
		t.Errorf("rows = %+v", rows)
	}
	if rows[0].Pool != "a" || !rows[0].Relevant {
		t.Errorf("row 1 = %+v", rows[0])
	}
}

func TestEvaluateStopsWhenOllamaIsGone(t *testing.T) {
	entries := []Entry{{"1", ""}, {"2", ""}}
	papers := map[string]pubmed.Article{"1": {PMID: "1", Abstract: "A"}, "2": {PMID: "2", Abstract: "A"}}
	labels := map[string]Label{"1": {Relevant: true}, "2": {Relevant: true}}
	s := &fakeSummarizer{errs: map[string]error{"1": fmt.Errorf("dial: %w", llm.ErrUnavailable)}}
	if _, err := Evaluate(context.Background(), s, entries, papers, labels, nil); !errors.Is(err, llm.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if len(s.calls) != 1 {
		t.Errorf("kept calling the model after it went away: %v", s.calls)
	}
}

func TestPapersCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "papers.json")
	if got, err := LoadPapers(path); err != nil || len(got) != 0 {
		t.Fatalf("missing cache: %v, %v", got, err)
	}
	in := map[string]pubmed.Article{"2": {PMID: "2", Title: "B"}, "1": {PMID: "1", Title: "A"}}
	if err := SavePapers(path, in); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPapers(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got %v", got)
	}
}

// The bench table is pinned the same way as the report, and shared with TS.
func TestBenchGolden(t *testing.T) {
	rows := sampleRows()
	worse := sampleRows()
	worse[0].Score = 2 // the small model misses the true positive
	worse[1].Usage.Repaired = true
	bench := []BenchRow{
		{Model: "llama3.2:1b", SizeBytes: 1321098329, LoadMS: 1240, Report: Report{PromptVersion: "v1", Format: "schema", Threshold: 4, Rows: worse}},
		{Model: "llama3.2:3b", SizeBytes: 2019393189, LoadMS: 2100, Report: Report{PromptVersion: "v1", Format: "schema", Threshold: 4, Rows: rows}},
	}
	got := BenchMarkdown(bench)
	path := filepath.Join("testdata", "bench.golden.md")
	want, err := os.ReadFile(path)
	if *update || os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s; review it before committing", path)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("bench table differs from %s (run with -update if intended):\n%s", path, got)
	}
}

func TestGigabytes(t *testing.T) {
	for b, want := range map[int64]string{2019393189: "2.0 GB", 1321098329: "1.3 GB", 4920753328: "4.9 GB", 0: "0.0 GB"} {
		if got := gigabytes(b); got != want {
			t.Errorf("gigabytes(%d) = %q, want %q", b, got, want)
		}
	}
}
