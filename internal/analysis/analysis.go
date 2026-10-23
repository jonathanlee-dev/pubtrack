// Package analysis renders the eval-set report for the three model features
// beyond relevance scoring: summary fact-checking, structured extraction, and
// similarity to the reader's own papers.
package analysis

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"pubtrack/internal/digest"
	"pubtrack/internal/eval"
	"pubtrack/internal/extract"
	"pubtrack/internal/factcheck"
	"pubtrack/internal/llm"
	"pubtrack/internal/similarity"
)

// Thresholds for the similarity pre-filter table. nomic-embed-text scores
// bunch together toward the top of the range, so the cutoffs do too.
var Thresholds = []float64{0.65, 0.70, 0.75, 0.80, 0.85}

type Row struct {
	PMID  string `json:"pmid"`
	Pool  string `json:"pool,omitempty"`
	Title string `json:"title"`
	// Labeled is false when labels.csv has no relevance label for the paper.
	Labeled  bool `json:"labeled"`
	Relevant bool `json:"relevant"`

	Summary      string             `json:"summary,omitempty"`
	SummaryError string             `json:"summary_error,omitempty"`
	Verdict      *factcheck.Verdict `json:"factcheck,omitempty"`
	JudgeError   string             `json:"judge_error,omitempty"`
	Fields       *extract.Fields    `json:"extraction,omitempty"`
	ExtractError string             `json:"extract_error,omitempty"`
	Similarity   *similarity.Match  `json:"similarity,omitempty"`
}

// Totals sums every model call (summaries, extraction, and judging).
type Totals struct {
	Calls        int   `json:"calls"`
	PromptTokens int   `json:"prompt_tokens"`
	OutputTokens int   `json:"output_tokens"`
	Millis       int64 `json:"ms"`
}

func (t *Totals) Add(u llm.Usage) {
	t.Calls += u.Attempts
	t.PromptTokens += u.PromptTokens
	t.OutputTokens += u.OutputTokens
	t.Millis += u.LatencyMS
}

type Analysis struct {
	Implementation   string                   `json:"implementation"`
	Date             string                   `json:"date"`
	Model            string                   `json:"model"`
	Judge            string                   `json:"judge"`
	EmbeddingModel   string                   `json:"embedding_model"`
	OwnPapers        int                      `json:"own_papers"`
	Rows             []Row                    `json:"rows"`
	ExtractionLabels map[string]extract.Label `json:"-"`
	Totals           Totals                   `json:"totals"`
}

// Markdown renders the report. It is pinned by testdata/analysis.golden.md.
func (a Analysis) Markdown() string {
	var b strings.Builder
	labeled := 0
	for _, r := range a.Rows {
		if r.Labeled {
			labeled++
		}
	}
	fmt.Fprintf(&b, "# pubtrack analysis — %s\n\n", a.Date)
	fmt.Fprintf(&b, "Eval set: %d papers (%d labeled for relevance) · model `%s` · judge `%s` · embeddings `%s`\n",
		len(a.Rows), labeled, a.Model, a.Judge, a.EmbeddingModel)
	factcheckSection(&b, a.Judge, a.Rows)
	a.extractionSection(&b)
	a.similaritySection(&b)
	totalsSection(&b, a.Totals)
	return b.String()
}

// FactcheckReport is the fact-check section on its own, for a weekly digest.
func FactcheckReport(title, judge string, rows []Row, t Totals) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Fact-check — %s\n", title)
	factcheckSection(&b, judge, rows)
	totalsSection(&b, t)
	return b.String()
}

func factcheckSection(b *strings.Builder, judge string, rows []Row) {
	faithful, noSummary := 0, 0
	var flagged, failed []Row
	for _, r := range rows {
		switch {
		case r.Summary == "":
			noSummary++
		case r.JudgeError != "":
			failed = append(failed, r)
		case r.Verdict != nil && r.Verdict.Supported:
			faithful++
		case r.Verdict != nil:
			flagged = append(flagged, r)
		}
	}
	fmt.Fprintf(b, "\n## Summary fact-check\n\n`%s` checked each summary against its abstract. "+
		"A flag is the judge's opinion, not ground truth: read the abstract before trusting it.\n\n", judge)
	b.WriteString("| Checked | Faithful | Flagged | Judge failed | No summary |\n|---|---|---|---|---|\n")
	fmt.Fprintf(b, "| %d | %d | %d | %d | %d |\n", faithful+len(flagged), faithful, len(flagged), len(failed), noSummary)
	if len(flagged) > 0 {
		fmt.Fprintf(b, "\n### Flagged (%d)\n", len(flagged))
		for _, r := range flagged {
			fmt.Fprintf(b, "\n#### %s\n\n%s\n\n", link(r, false), r.Summary)
			for _, c := range r.Verdict.Unsupported {
				fmt.Fprintf(b, "- %s\n", c)
			}
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(b, "\n### Judge failed (%d)\n\n", len(failed))
		for _, r := range failed {
			fmt.Fprintf(b, "- %s — %s\n", link(r, false), r.JudgeError)
		}
	}
}

func (a Analysis) extractionSection(b *strings.Builder) {
	b.WriteString("\n## Structured extraction\n\n")
	var order []string
	got := map[string]extract.Fields{}
	byPMID := map[string]Row{}
	for _, r := range a.Rows {
		order = append(order, r.PMID)
		byPMID[r.PMID] = r
		if r.Fields != nil {
			got[r.PMID] = *r.Fields
		}
	}
	if len(a.ExtractionLabels) == 0 {
		b.WriteString("No extraction labels yet. Fill in `eval/extraction_labels.csv` to score this section.\n")
	} else {
		s := extract.Score(order, got, a.ExtractionLabels)
		b.WriteString("| Field | Correct | Labeled | Accuracy |\n|---|---|---|---|\n")
		for _, f := range []struct {
			name string
			t    extract.Tally
		}{{"Study design", s.Design}, {"Sample size", s.Size}, {"Setting", s.Setting}} {
			fmt.Fprintf(b, "| %s | %d | %d | %s |\n", f.name, f.t.Correct, f.t.Labeled, eval.Ratio(f.t.Correct, f.t.Labeled))
		}
		if len(s.Mismatches) > 0 {
			fmt.Fprintf(b, "\n### Mismatches (%d)\n\n", len(s.Mismatches))
			for _, m := range s.Mismatches {
				fmt.Fprintf(b, "- %s — %s: labeled `%s`, extracted `%s`\n", link(byPMID[m.PMID], false), m.Field, m.Labeled, m.Extracted)
			}
		}
	}

	b.WriteString("\n### Extracted fields\n\n| Paper | Design | n | Setting | Technology |\n|---|---|---|---|---|\n")
	for _, r := range a.Rows {
		switch {
		case r.Fields != nil:
			f := r.Fields
			n, tech := "—", "—"
			if f.SampleSize > 0 {
				n = strconv.Itoa(f.SampleSize)
			}
			if len(f.Technology) > 0 {
				tech = strings.Join(f.Technology, ", ")
			}
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", link(r, true), cell(f.StudyDesign), n, cell(f.Setting), cell(tech))
		case r.ExtractError != "":
			fmt.Fprintf(b, "| %s | failed: %s | — | — | — |\n", link(r, true), cell(r.ExtractError))
		}
	}
}

func (a Analysis) similaritySection(b *strings.Builder) {
	fmt.Fprintf(b, "\n## Similarity to your papers\n\nScore = highest cosine similarity between a paper and any of your %d papers, using `%s`.\n\n",
		a.OwnPapers, a.EmbeddingModel)
	var scored []Row
	var labeled []similarity.Scored
	for _, r := range a.Rows {
		if r.Similarity == nil {
			continue
		}
		scored = append(scored, r)
		if r.Labeled {
			labeled = append(labeled, similarity.Scored{Score: r.Similarity.Score, Relevant: r.Relevant})
		}
	}
	if len(scored) == 0 {
		b.WriteString("Similarity wasn't computed.\n")
		return
	}

	sep := similarity.Separate(labeled, Thresholds)
	if sep.Relevant > 0 && sep.NotRelevant > 0 {
		fmt.Fprintf(b, "Mean score: relevant %s · not relevant %s · ranking AUC %s "+
			"(0.50 is chance; 1.00 means every relevant paper outscores every other)\n\n",
			similarity.Fixed2(sep.MeanRelevant), similarity.Fixed2(sep.MeanNotRelevant), eval.Ratio(sep.AUCHalves, 2*sep.Pairs))
		b.WriteString("| Keep if ≥ | Kept | Relevant kept | Recall | Precision |\n|---|---|---|---|---|\n")
		for _, c := range sep.Cutoffs {
			fmt.Fprintf(b, "| %s | %d | %d/%d | %s | %s |\n", similarity.Fixed2(c.Threshold), c.Kept, c.RelevantKept, sep.Relevant,
				eval.Ratio(c.RelevantKept, sep.Relevant), eval.Ratio(c.RelevantKept, c.Kept))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Label relevance in `eval/labels.csv` (both relevant and not-relevant papers) to see how well similarity separates them.\n\n")
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Similarity.Score != scored[j].Similarity.Score {
			return scored[i].Similarity.Score > scored[j].Similarity.Score
		}
		return pmidLess(scored[i].PMID, scored[j].PMID)
	})
	b.WriteString("| Paper | Label | Score | Closest |\n|---|---|---|---|\n")
	for _, r := range scored {
		label := "—"
		if r.Labeled {
			label = "n"
			if r.Relevant {
				label = "y"
			}
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", link(r, true), label, similarity.Fixed2(r.Similarity.Score), cell(r.Similarity.Closest))
	}
}

func totalsSection(b *strings.Builder, t Totals) {
	b.WriteString("\n## Run summary\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| Model calls | %d |\n", t.Calls)
	fmt.Fprintf(b, "| Tokens in / out | %s / %s |\n", digest.Commas(t.PromptTokens), digest.Commas(t.OutputTokens))
	fmt.Fprintf(b, "| Model time | %s |\n", digest.Seconds(t.Millis))
}

const shortTitle = 60

// link renders a markdown link to the paper. In a table cell the title is
// shortened and pipes are escaped so they don't split the cell.
func link(r Row, inTable bool) string {
	title := strings.TrimSpace(r.Title)
	if title == "" {
		title = "PMID " + r.PMID
	}
	if inTable {
		if runes := []rune(title); len(runes) > shortTitle {
			title = string(runes[:shortTitle]) + "…"
		}
		title = cell(title)
	}
	title = strings.NewReplacer("[", `\[`, "]", `\]`).Replace(title)
	return fmt.Sprintf("[%s](https://pubmed.ncbi.nlm.nih.gov/%s/)", title, r.PMID)
}

func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

func pmidLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
