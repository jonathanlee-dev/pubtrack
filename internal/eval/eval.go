// Package eval scores the model's relevance calls against hand labels.
//
// Inputs live in one directory: pmids.txt (the eval set and the pool each
// paper came from), labels.csv (the reader's y/n labels), and papers.json (a
// local cache of the PubMed records, so evals don't depend on NCBI and every
// run scores identical text).
package eval

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"pubtrack/internal/digest"
	"pubtrack/internal/llm"
	"pubtrack/internal/pubmed"
)

type Entry struct {
	PMID string
	Pool string
}

// ReadPMIDs reads pmids.txt: one "PMID<TAB>pool" per line; # starts a comment.
func ReadPMIDs(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		pmid := strings.TrimSpace(fields[0])
		if _, err := strconv.Atoi(pmid); err != nil {
			return nil, fmt.Errorf("%s:%d: %q is not a PMID", path, i+1, pmid)
		}
		e := Entry{PMID: pmid}
		if len(fields) > 1 {
			e.Pool = strings.TrimSpace(fields[1])
		}
		out = append(out, e)
	}
	return out, nil
}

type Label struct {
	Relevant bool
	Note     string
}

// ReadLabels reads labels.csv (columns include pmid, relevant, and optionally
// note). A blank relevant cell means not labeled yet; those are counted, not
// returned.
func ReadLabels(path string) (map[string]Label, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	if len(records) == 0 {
		return nil, 0, fmt.Errorf("%s is empty", path)
	}

	col := map[string]int{}
	for i, h := range records[0] {
		// A spreadsheet app may save a byte-order mark before the first header.
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	if _, ok := col["pmid"]; !ok {
		return nil, 0, fmt.Errorf("%s: no pmid column", path)
	}
	if _, ok := col["relevant"]; !ok {
		return nil, 0, fmt.Errorf("%s: no relevant column", path)
	}

	labels := map[string]Label{}
	unlabeled := 0
	for i, rec := range records[1:] {
		get := func(name string) string {
			j, ok := col[name]
			if !ok || j >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[j])
		}
		pmid := get("pmid")
		if pmid == "" {
			continue
		}
		var relevant bool
		switch v := strings.ToLower(get("relevant")); v {
		case "":
			unlabeled++
			continue
		case "y", "yes", "1", "true":
			relevant = true
		case "n", "no", "0", "false":
			relevant = false
		default:
			return nil, 0, fmt.Errorf("%s line %d: relevant is %q; use y or n", path, i+2, get("relevant"))
		}
		labels[pmid] = Label{Relevant: relevant, Note: get("note")}
	}
	return labels, unlabeled, nil
}

// LoadPapers reads the papers.json cache. A missing file is an empty cache.
func LoadPapers(path string) (map[string]pubmed.Article, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]pubmed.Article{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []pubmed.Article
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]pubmed.Article, len(list))
	for _, a := range list {
		out[a.PMID] = a
	}
	return out, nil
}

// SavePapers writes the cache sorted by PMID, so re-saving doesn't reorder it.
func SavePapers(path string, papers map[string]pubmed.Article) error {
	list := make([]pubmed.Article, 0, len(papers))
	for _, a := range papers {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return pmidLess(list[i].PMID, list[j].PMID) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

type Row struct {
	PMID     string    `json:"pmid"`
	Pool     string    `json:"pool,omitempty"`
	Title    string    `json:"title"`
	Relevant bool      `json:"labeled_relevant"`
	Note     string    `json:"note,omitempty"`
	Score    int       `json:"score,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Error    string    `json:"error,omitempty"`
	Usage    llm.Usage `json:"usage"`
}

type Summarizer interface {
	Summarize(ctx context.Context, p llm.Paper) (llm.Result, llm.Usage, error)
}

// Evaluate scores every labeled paper, in pmids.txt order. A paper whose
// reply can't be used is recorded as a failure; Ollama going away stops the
// eval, because the rest would fail the same way.
func Evaluate(ctx context.Context, s Summarizer, entries []Entry, papers map[string]pubmed.Article, labels map[string]Label, progress io.Writer) ([]Row, error) {
	var rows []Row
	for _, e := range entries {
		lab, ok := labels[e.PMID]
		if !ok {
			continue
		}
		row := Row{PMID: e.PMID, Pool: e.Pool, Relevant: lab.Relevant, Note: lab.Note}
		a, ok := papers[e.PMID]
		if !ok {
			row.Error = "not in papers.json"
			rows = append(rows, row)
			continue
		}
		row.Title = a.Title
		res, u, err := s.Summarize(ctx, llm.Paper{PMID: a.PMID, Title: a.Title, Journal: a.Journal, Abstract: a.Abstract})
		row.Usage = u
		switch {
		case err == nil:
			row.Score, row.Reason, row.Summary = res.Relevance, res.Reason, res.Summary
		case ctx.Err() != nil:
			return rows, ctx.Err()
		case errors.Is(err, llm.ErrUnavailable):
			return rows, err
		default:
			row.Error = err.Error()
		}
		if progress != nil {
			status := fmt.Sprintf("scored %d", row.Score)
			if row.Error != "" {
				status = "failed: " + row.Error
			}
			fmt.Fprintf(progress, "[%d/%d] %s %s\n", len(rows)+1, len(labels), e.PMID, status)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Metrics is the confusion matrix for "relevant = score >= threshold".
// Failed rows are counted separately and left out of the matrix.
type Metrics struct {
	TP     int `json:"true_positive"`
	FP     int `json:"false_positive"`
	FN     int `json:"false_negative"`
	TN     int `json:"true_negative"`
	Failed int `json:"failed"`
}

func Compute(rows []Row, threshold int) Metrics {
	var m Metrics
	for _, r := range rows {
		switch said := r.Score >= threshold; {
		case r.Error != "":
			m.Failed++
		case said && r.Relevant:
			m.TP++
		case said && !r.Relevant:
			m.FP++
		case !said && r.Relevant:
			m.FN++
		default:
			m.TN++
		}
	}
	return m
}

type Report struct {
	Implementation string  `json:"implementation"`
	Date           string  `json:"date"`
	Model          string  `json:"model"`
	PromptVersion  string  `json:"prompt_version"`
	Format         string  `json:"format"`
	Threshold      int     `json:"threshold"`
	Unlabeled      int     `json:"unlabeled"`
	Metrics        Metrics `json:"metrics"`
	Rows           []Row   `json:"rows"`
}

func (r Report) JSON() ([]byte, error) {
	r.Metrics = Compute(r.Rows, r.Threshold)
	if r.Rows == nil {
		r.Rows = []Row{}
	}
	return json.MarshalIndent(r, "", "  ")
}

// Text renders the report. It is pinned by testdata/report.golden.txt.
func (r Report) Text() string {
	m := Compute(r.Rows, r.Threshold)
	t := r.Threshold
	var b strings.Builder

	fmt.Fprintf(&b, "pubtrack eval · model %s · prompt %s · format %s · relevant = score ≥ %d\n\n", r.Model, r.PromptVersion, r.Format, t)
	relevant := 0
	for _, row := range r.Rows {
		if row.Relevant {
			relevant++
		}
	}
	fmt.Fprintf(&b, "Labeled %d (%d relevant, %d not) · scored %d · failed %d · unlabeled %d\n\n",
		len(r.Rows), relevant, len(r.Rows)-relevant, len(r.Rows)-m.Failed, m.Failed, r.Unlabeled)

	fmt.Fprintf(&b, "  %-10s %10s %10s\n", "", "labeled y", "labeled n")
	fmt.Fprintf(&b, "  %-10s %10d %10d\n", fmt.Sprintf("model ≥ %d", t), m.TP, m.FP)
	fmt.Fprintf(&b, "  %-10s %10d %10d\n\n", fmt.Sprintf("model < %d", t), m.FN, m.TN)

	fmt.Fprintf(&b, "  %-10s %s  (%d/%d)\n", "precision", Ratio(m.TP, m.TP+m.FP), m.TP, m.TP+m.FP)
	fmt.Fprintf(&b, "  %-10s %s  (%d/%d)\n", "recall", Ratio(m.TP, m.TP+m.FN), m.TP, m.TP+m.FN)
	fmt.Fprintf(&b, "  %-10s %s  (%d/%d)\n\n", "agreement", Ratio(m.TP+m.TN, len(r.Rows)-m.Failed), m.TP+m.TN, len(r.Rows)-m.Failed)

	// The full distribution shows what a different threshold would do,
	// without changing the one that was fixed before labeling.
	b.WriteString("Score distribution\n")
	fmt.Fprintf(&b, "  %5s %10s %10s\n", "score", "labeled y", "labeled n")
	for score := 5; score >= 1; score-- {
		y, n := 0, 0
		for _, row := range r.Rows {
			if row.Error == "" && row.Score == score {
				if row.Relevant {
					y++
				} else {
					n++
				}
			}
		}
		fmt.Fprintf(&b, "  %5d %10d %10d\n", score, y, n)
	}
	b.WriteString("\n")

	var wrong, failed []Row
	for _, row := range r.Rows {
		switch {
		case row.Error != "":
			failed = append(failed, row)
		case (row.Score >= t) != row.Relevant:
			wrong = append(wrong, row)
		}
	}
	byPMID := func(rows []Row) {
		sort.Slice(rows, func(i, j int) bool { return pmidLess(rows[i].PMID, rows[j].PMID) })
	}
	byPMID(wrong)
	byPMID(failed)

	fmt.Fprintf(&b, "Disagreements (%d)\n", len(wrong))
	for _, row := range wrong {
		yn := "n"
		if row.Relevant {
			yn = "y"
		}
		fmt.Fprintf(&b, "  %s  labeled %s, scored %d%s\n", row.PMID, yn, row.Score, pool(row))
		fmt.Fprintf(&b, "    %s\n", row.Title)
		fmt.Fprintf(&b, "    Model: %s\n", row.Reason)
		if row.Note != "" {
			fmt.Fprintf(&b, "    Your note: %s\n", row.Note)
		}
	}
	b.WriteString("\n")

	if len(failed) > 0 {
		fmt.Fprintf(&b, "Failed (%d)\n", len(failed))
		for _, row := range failed {
			fmt.Fprintf(&b, "  %s%s  %s\n", row.PMID, pool(row), row.Error)
		}
		b.WriteString("\n")
	}

	var in, out, calls int
	var ms int64
	for _, row := range r.Rows {
		in += row.Usage.PromptTokens
		out += row.Usage.OutputTokens
		ms += row.Usage.LatencyMS
		if row.Usage.Attempts > 0 {
			calls++
		}
	}
	fmt.Fprintf(&b, "Tokens in / out %s / %s · LLM time %s", digest.Commas(in), digest.Commas(out), digest.Seconds(ms))
	if calls > 0 {
		fmt.Fprintf(&b, " (avg %s per paper)", digest.Seconds(ms/int64(calls)))
	}
	b.WriteString("\n")
	return b.String()
}

func pool(row Row) string {
	if row.Pool == "" {
		return ""
	}
	return "  [" + row.Pool + "]"
}

// Ratio formats num/den to two places, rounding half up with integer math so
// a half always rounds up, whatever its binary value.
func Ratio(num, den int) string {
	if den == 0 {
		return "n/a"
	}
	v := (num*200 + den) / (2 * den)
	return fmt.Sprintf("%d.%02d", v/100, v%100)
}

func pmidLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
