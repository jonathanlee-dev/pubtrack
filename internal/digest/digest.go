// Package digest renders a run's results as markdown (to read) and JSON
// (for the Phase 2 viewer).
package digest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"pubtrack/internal/extract"
	"pubtrack/internal/llm"
	"pubtrack/internal/pubmed"
	"pubtrack/internal/similarity"
)

type Item struct {
	Article  pubmed.Article `json:"article"`
	Topics   []string       `json:"topics,omitempty"`
	Cites    []string       `json:"cites_own_work,omitempty"`
	Mentions []string       `json:"mentions_own_platform,omitempty"`
	Result   *llm.Result    `json:"result,omitempty"`
	Usage    *llm.Usage     `json:"usage,omitempty"`
	Note     string         `json:"note,omitempty"`
	Error    string         `json:"error,omitempty"`

	Extraction   *extract.Fields   `json:"extraction,omitempty"`
	ExtractError string            `json:"extract_error,omitempty"`
	Similarity   *similarity.Match `json:"similarity,omitempty"`
}

type Stats struct {
	Candidates    int   `json:"candidates"`
	Summarized    int   `json:"summarized"`
	NoAbstract    int   `json:"no_abstract"`
	Failed        int   `json:"failed"`
	SkippedErrata int   `json:"skipped_errata"`
	Duplicates    int   `json:"duplicates_skipped"`
	Baselined     int   `json:"baselined"`
	LLMPapers     int   `json:"llm_papers"`
	LLMCalls      int   `json:"llm_calls"`
	PromptTokens  int   `json:"prompt_tokens"`
	OutputTokens  int   `json:"output_tokens"`
	LLMMillis     int64 `json:"llm_ms"`
	WarmupMillis  int64 `json:"warmup_ms"`
	Repaired      int   `json:"repaired"`
	Truncated     int   `json:"truncated"`
	ContextFull   int   `json:"context_full"`
	Extracted     int   `json:"extracted"`
	ExtractFailed int   `json:"extract_failed"`
	Similarities  int   `json:"similarity_scored"`
}

func (s *Stats) AddUsage(u llm.Usage) {
	s.LLMPapers++
	s.LLMCalls += u.Attempts
	s.PromptTokens += u.PromptTokens
	s.OutputTokens += u.OutputTokens
	s.LLMMillis += u.LatencyMS
	if u.Repaired {
		s.Repaired++
	}
	if u.Truncated {
		s.Truncated++
	}
	if u.ContextFull {
		s.ContextFull++
	}
}

// AddExtraUsage counts a follow-up call for a paper already counted (field
// extraction): its tokens, time, and calls, but not another paper.
func (s *Stats) AddExtraUsage(u llm.Usage) {
	s.LLMCalls += u.Attempts
	s.PromptTokens += u.PromptTokens
	s.OutputTokens += u.OutputTokens
	s.LLMMillis += u.LatencyMS
	if u.ContextFull {
		s.ContextFull++
	}
}

type Digest struct {
	Date          string   `json:"date"`
	WindowDays    int      `json:"window_days"`
	Model         string   `json:"model"`
	PromptVersion string   `json:"prompt_version"`
	Items         []Item   `json:"items"`
	Warnings      []string `json:"warnings,omitempty"`
	Stats         Stats    `json:"stats"`
}

func (d Digest) JSON() ([]byte, error) {
	if d.Items == nil {
		d.Items = []Item{}
	}
	return json.MarshalIndent(d, "", "  ")
}

// Markdown renders the digest. Output depends only on the content, not the
// order of Items, so re-rendering the same run gives the same file.
func (d Digest) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# pubtrack digest — %s\n\n", d.Date)
	fmt.Fprintf(&b, "Papers added to PubMed in the last %d days · model `%s` · prompt `%s`\n", d.WindowDays, d.Model, d.PromptVersion)

	var citing, other, failed []Item
	for _, it := range d.Items {
		switch {
		case it.Error != "":
			failed = append(failed, it)
		case len(it.Cites) > 0:
			citing = append(citing, it)
		default:
			other = append(other, it)
		}
	}
	sortByRelevance(citing)
	sortByRelevance(other)
	sort.Slice(failed, func(i, j int) bool { return failed[i].Article.PMID < failed[j].Article.PMID })

	if len(d.Items) == 0 {
		b.WriteString("\nNo new papers this run.\n")
	}
	writeSection(&b, "Cites your work", citing)
	writeSection(&b, "New papers", other)
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n## Not summarized (%d)\n\n", len(failed))
		for _, it := range failed {
			fmt.Fprintf(&b, "- %s — %s\n", link(it.Article), it.Error)
		}
	}
	if len(d.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, w := range d.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	writeStats(&b, d.Stats)
	return b.String()
}

func relevance(it Item) int {
	if it.Result == nil {
		return 0
	}
	return it.Result.Relevance
}

// sortByRelevance orders by score (unscored last), then newest PMID first.
func sortByRelevance(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		ri, rj := relevance(items[i]), relevance(items[j])
		if ri != rj {
			return ri > rj
		}
		return pmidLess(items[j].Article.PMID, items[i].Article.PMID)
	})
}

func pmidLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func writeSection(b *strings.Builder, title string, items []Item) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s (%d)\n", title, len(items))
	for _, it := range items {
		writeItem(b, it)
	}
}

func writeItem(b *strings.Builder, it Item) {
	a := it.Article
	fmt.Fprintf(b, "\n### %s\n\n", link(a))

	var meta []string
	if a.Journal != "" {
		meta = append(meta, "*"+a.Journal+"*")
	}
	if a.Published != "" {
		meta = append(meta, a.Published)
	}
	if s := authors(a.Authors); s != "" {
		meta = append(meta, s)
	}
	if a.DOI != "" {
		// Angle brackets keep parentheses inside DOIs from ending the link.
		meta = append(meta, fmt.Sprintf("[doi:%s](<https://doi.org/%s>)", a.DOI, a.DOI))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n\n")
	}

	switch {
	case it.Result != nil:
		fmt.Fprintf(b, "**Relevance %d/5** — %s\n\n%s\n", it.Result.Relevance, it.Result.Reason, it.Result.Summary)
	case it.Note != "":
		fmt.Fprintf(b, "*%s*\n", it.Note)
	}
	if it.Extraction != nil {
		fmt.Fprintf(b, "\n%s\n", extract.Line(*it.Extraction))
	}
	if it.Similarity != nil {
		fmt.Fprintf(b, "\nSimilar to your work: %s (closest: %s)\n", similarity.Fixed2(it.Similarity.Score), it.Similarity.Closest)
	}

	var tags []string
	if len(it.Topics) > 0 {
		tags = append(tags, "Topics: "+strings.Join(it.Topics, ", "))
	}
	if len(it.Cites) > 0 {
		tags = append(tags, "Cites: "+strings.Join(it.Cites, ", "))
	}
	if len(it.Mentions) > 0 {
		tags = append(tags, "Mentions: "+strings.Join(it.Mentions, ", "))
	}
	if len(tags) > 0 {
		b.WriteString("\n" + strings.Join(tags, " · ") + "\n")
	}
}

func link(a pubmed.Article) string {
	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = "PMID " + a.PMID
	}
	title = strings.NewReplacer("[", `\[`, "]", `\]`).Replace(title)
	return fmt.Sprintf("[%s](%s)", title, a.URL())
}

func authors(list []string) string {
	if len(list) <= 3 {
		return strings.Join(list, ", ")
	}
	return strings.Join(list[:3], ", ") + ", et al."
}

func writeStats(b *strings.Builder, s Stats) {
	b.WriteString("\n## Run summary\n\n| | |\n|---|---|\n")
	row := func(label, value string) { fmt.Fprintf(b, "| %s | %s |\n", label, value) }
	count := func(label string, n int) { row(label, strconv.Itoa(n)) }

	count("New papers found", s.Candidates)
	count("Summarized", s.Summarized)
	if s.NoAbstract > 0 {
		count("No abstract (not scored)", s.NoAbstract)
	}
	count("Failed", s.Failed)
	if s.SkippedErrata > 0 {
		count("Errata skipped", s.SkippedErrata)
	}
	if s.Duplicates > 0 {
		count("Duplicates skipped (same title or DOI as an earlier paper)", s.Duplicates)
	}
	if s.Baselined > 0 {
		count("Existing citing papers recorded (first run)", s.Baselined)
	}
	row("Tokens in / out", Commas(s.PromptTokens)+" / "+Commas(s.OutputTokens))
	llmTime := Seconds(s.LLMMillis)
	if s.LLMPapers > 0 {
		llmTime += fmt.Sprintf(" (avg %s per paper)", Seconds(s.LLMMillis/int64(s.LLMPapers)))
	}
	row("LLM time", llmTime)
	if s.WarmupMillis > 0 {
		row("Model load (warm-up)", Seconds(s.WarmupMillis))
	}
	count("LLM calls (incl. retries)", s.LLMCalls)
	count("Output needed repair", s.Repaired)
	count("Abstracts truncated", s.Truncated)
	if s.ContextFull > 0 {
		count("Prompts that filled num_ctx (raise it)", s.ContextFull)
	}
	if n := s.Extracted + s.ExtractFailed; n > 0 {
		row("Fields extracted", fmt.Sprintf("%d of %d", s.Extracted, n))
	}
	if s.Similarities > 0 {
		count("Similarity scored", s.Similarities)
	}
}

// Seconds formats milliseconds as "1.3s", rounding half up with integer math.
// %.1f rounds half to even on the binary value, so 1250ms would print 1.2s.
func Seconds(ms int64) string {
	tenths := (ms + 50) / 100
	return fmt.Sprintf("%d.%ds", tenths/10, tenths%10)
}

// Commas formats 2400 as "2,400" (fixed format, not locale-dependent).
func Commas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + Commas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
