package digest

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pubtrack/internal/extract"
	"pubtrack/internal/llm"
	"pubtrack/internal/pubmed"
	"pubtrack/internal/similarity"
)

var update = flag.Bool("update", false, "rewrite golden files")

func sample() Digest {
	return Digest{
		Date: "2026-10-19", WindowDays: 10, Model: "llama3.2:3b", PromptVersion: "v1",
		Items: []Item{
			{
				Article: pubmed.Article{PMID: "100", Title: "Low relevance title [pilot]", Journal: "J Aging", Published: "2026-10-01", Authors: []string{"Smith J"}},
				Topics:  []string{"digital_biomarkers"},
				Result:  &llm.Result{Summary: "Summary one.", Relevance: 2, Reason: "Reason one."},
				Extraction: &extract.Fields{
					StudyDesign: "review", Setting: "not stated", Population: "older adults", Technology: []string{},
				},
				Similarity: &similarity.Match{Score: 0.4, Closest: "Gerontology 2021 (SMART)"},
			},
			{
				Article: pubmed.Article{PMID: "200", Title: "High relevance title", Journal: "Gerontology", DOI: "10.1016/S1474-4422(12)70191-6", Authors: []string{"Ames A", "Burr B", "Cole C", "Dunn D"}},
				Topics:  []string{"in_home_sensing", "digital_biomarkers"},
				Result:  &llm.Result{Summary: "Summary two.", Relevance: 5, Reason: "Reason two."},
				Extraction: &extract.Fields{
					StudyDesign: "observational", SampleSize: 186, Setting: "home",
					Population: "community-dwelling adults 75+", Technology: []string{"wearable accelerometer", "motion sensors"},
				},
				Similarity: &similarity.Match{Score: 0.7149, Closest: "JMIR Form Res 2020 (veterans in-home sensing)"},
			},
			{
				Article:  pubmed.Article{PMID: "300", Title: "Paper that cites SMART"},
				Cites:    []string{"Gerontology 2021 (SMART)"},
				Mentions: []string{"ORCATECH"},
				Result:   &llm.Result{Summary: "Summary three.", Relevance: 3, Reason: "Reason three."},
			},
			{
				Article: pubmed.Article{PMID: "400"},
				Error:   "not returned by PubMed efetch (attempt 1 of 3; will retry next run)",
			},
			{
				Article: pubmed.Article{PMID: "500", Title: "Letter with no abstract"},
				Note:    "No abstract in PubMed, so not scored.",
			},
		},
		Warnings: []string{`query "own_platforms": PubMed ignored [erratum]`},
		Stats: Stats{
			Candidates: 5, Summarized: 3, NoAbstract: 1, Failed: 1,
			LLMPapers: 3, LLMCalls: 4, PromptTokens: 2400, OutputTokens: 300, LLMMillis: 9300, WarmupMillis: 2100,
			Extracted: 2, ExtractFailed: 1, Similarities: 2,
		},
	}
}

func TestMarkdownSectionsAndOrder(t *testing.T) {
	md := sample().Markdown()
	order := []string{
		"## Cites your work (1)", "Paper that cites SMART",
		"## New papers (3)", "High relevance title", "Low relevance title", "Letter with no abstract",
		"## Not summarized (1)", "PMID 400",
		"## Warnings",
		"## Run summary",
	}
	last := -1
	for _, s := range order {
		i := strings.Index(md, s)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", s, md)
		}
		if i < last {
			t.Errorf("%q is out of order", s)
		}
		last = i
	}
	for _, want := range []string{
		`Low relevance title \[pilot\]`,                     // brackets escaped inside link text
		"(<https://doi.org/10.1016/S1474-4422(12)70191-6>)", // parens in a DOI don't break the link
		"Ames A, Burr B, Cole C, et al.",
		"**Relevance 5/5** — Reason two.",
		"Topics: in_home_sensing, digital_biomarkers",
		"Cites: Gerontology 2021 (SMART) · Mentions: ORCATECH",
		"| Tokens in / out | 2,400 / 300 |",
		"| LLM time | 9.3s (avg 3.1s per paper) |",
		"| LLM calls (incl. retries) | 4 |",
		"Design: observational · n = 186 · Setting: home · Population: community-dwelling adults 75+ · Technology: wearable accelerometer, motion sensors",
		"Similar to your work: 0.71 (closest: JMIR Form Res 2020 (veterans in-home sensing))",
		"Design: review · n not stated · Setting: not stated · Population: older adults · Technology: none stated",
		"| Fields extracted | 2 of 3 |",
		"| Similarity scored | 2 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestMarkdownIsDeterministic(t *testing.T) {
	a := sample()
	b := sample()
	for i, j := 0, len(b.Items)-1; i < j; i, j = i+1, j-1 {
		b.Items[i], b.Items[j] = b.Items[j], b.Items[i]
	}
	if a.Markdown() != b.Markdown() {
		t.Error("item order in the input changed the output")
	}
}

func TestMarkdownEmpty(t *testing.T) {
	md := Digest{Date: "2026-10-19", WindowDays: 10}.Markdown()
	if !strings.Contains(md, "No new papers") {
		t.Errorf("empty digest:\n%s", md)
	}
}

func TestJSONEmptyItemsIsArray(t *testing.T) {
	b, err := Digest{}.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"items": []`) {
		t.Errorf("items should be [] not null:\n%s", b)
	}
}

// The golden file pins the full rendering. Regenerate after an intended
// change with: go test ./internal/digest -update
func TestMarkdownGolden(t *testing.T) {
	path := filepath.Join("testdata", "digest.golden.md")
	got := sample().Markdown()
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
		t.Errorf("markdown differs from %s (run with -update if the change is intended):\n%s", path, got)
	}
}
