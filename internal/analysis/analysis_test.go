package analysis

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pubtrack/internal/extract"
	"pubtrack/internal/factcheck"
	"pubtrack/internal/llm"
	"pubtrack/internal/similarity"
)

var update = flag.Bool("update", false, "rewrite golden files")

// sample is the analysis the golden files are rendered from.
func sample() Analysis {
	n40, n120 := 40, 120
	return Analysis{
		Implementation: "go", Date: "2026-10-19", Model: "llama3.2:3b", Judge: "llama3.1:8b", EmbeddingModel: "nomic-embed-text", OwnPapers: 6,
		Rows: []Row{
			{
				PMID: "101", Pool: "in_home_sensing", Title: "In-home gait sensing | pilot", Labeled: true, Relevant: true,
				Summary: "S1.", Verdict: &factcheck.Verdict{Supported: true},
				Fields:     &extract.Fields{StudyDesign: "observational", SampleSize: 40, Setting: "home", Population: "adults 70+", Technology: []string{"gait mat", "motion sensors"}},
				Similarity: &similarity.Match{Score: 0.834, Closest: "JMIR Form Res 2020 (veterans in-home sensing)"},
			},
			{
				PMID: "102", Pool: "remote_cognitive_assessment", Title: "Remote cognitive testing at scale with a very long title that needs to be shortened for the table", Labeled: true, Relevant: true,
				Summary: "The study enrolled 500 participants.", Verdict: &factcheck.Verdict{Unsupported: []string{"500 participants is not in the abstract"}},
				Fields:     &extract.Fields{StudyDesign: "validation", SampleSize: 120, Setting: "online", Population: "older adults", Technology: []string{"smartphone app"}},
				Similarity: &similarity.Match{Score: 0.78, Closest: "Gerontology 2021 (SMART)"},
			},
			{
				PMID: "103", Pool: "negative_cart", Title: "CAR-T outcomes [phase 2]", Labeled: true, Relevant: false,
				Summary: "S3.", JudgeError: `invalid model output: missing "verdict"`,
				ExtractError: `invalid model output: setting "moon" is not one of: home`,
				Similarity:   &similarity.Match{Score: 0.66, Closest: "Digit Biomark 2020 (CART)"},
			},
			{
				PMID: "104", Pool: "walking_reminiscence", Title: "Unlabeled paper",
				SummaryError: "Ollama /api/chat timed out after 2m0s",
				Similarity:   &similarity.Match{Score: 0.70, Closest: "Gerontologist 2025 (reminiscence walking)"},
			},
		},
		ExtractionLabels: map[string]extract.Label{
			"101": {StudyDesign: "observational", SampleSize: &n40, Setting: "home"},
			"102": {StudyDesign: "observational", SampleSize: &n120},
		},
		Totals: Totals{Calls: 9, PromptTokens: 7200, OutputTokens: 900, Millis: 21000},
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path)
	if *update || os.IsNotExist(err) {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
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
		t.Errorf("output differs from %s (run with -update if intended):\n%s", path, got)
	}
}

func TestMarkdownGolden(t *testing.T) {
	golden(t, "analysis.golden.md", sample().Markdown())
}

func TestFactcheckReportGolden(t *testing.T) {
	a := sample()
	golden(t, "factcheck.golden.md", FactcheckReport("digest-2026-10-19", a.Judge, a.Rows, a.Totals))
}

func TestMarkdownContents(t *testing.T) {
	md := sample().Markdown()
	for _, want := range []string{
		"| 2 | 1 | 1 | 1 | 1 |",                     // checked (= faithful + flagged), faithful, flagged, judge failed, no summary
		"- 500 participants is not in the abstract", // the flagged claim
		"| Study design | 1 | 2 | 0.50 |",           // 101 right, 102 wrong
		"| Sample size | 2 | 2 | 1.00 |",
		"design: labeled `observational`, extracted `validation`",
		`In-home gait sensing \| pilot`,                                 // pipes escaped in table cells
		"Remote cognitive testing at scale with a very long title tha…", // shortened to 60 characters
		"ranking AUC 1.00",                                              // both relevant papers outscore the one that isn't
		"| 0.80 | 1 | 1/2 | 0.50 | 1.00 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

func TestTotals(t *testing.T) {
	var tot Totals
	tot.Add(llm.Usage{Attempts: 2, PromptTokens: 10, OutputTokens: 5, LatencyMS: 300})
	tot.Add(llm.Usage{Attempts: 1, PromptTokens: 1, OutputTokens: 1, LatencyMS: 1})
	if tot != (Totals{Calls: 3, PromptTokens: 11, OutputTokens: 6, Millis: 301}) {
		t.Errorf("totals = %+v", tot)
	}
}
