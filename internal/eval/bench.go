package eval

import (
	"fmt"
	"strings"

	"pubtrack/internal/digest"
)

// BenchRow is one model's eval run within a benchmark.
type BenchRow struct {
	Model     string
	SizeBytes int64
	LoadMS    int64
	Report    Report
}

// BenchMarkdown renders the comparison as a markdown table, ready to paste
// into the README. It is pinned by testdata/bench.golden.md.
func BenchMarkdown(rows []BenchRow) string {
	if len(rows) == 0 {
		return "No models benchmarked.\n"
	}
	first := rows[0].Report
	var b strings.Builder
	fmt.Fprintf(&b, "## pubtrack bench — prompt %s · format %s · relevant = score ≥ %d\n\n", first.PromptVersion, first.Format, first.Threshold)
	b.WriteString("| Model | Size | Load | Avg per paper | Precision | Recall | Agreement | Failed | Repaired |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		m := Compute(r.Report.Rows, r.Report.Threshold)
		var ms int64
		calls, repaired := 0, 0
		for _, row := range r.Report.Rows {
			ms += row.Usage.LatencyMS
			if row.Usage.Attempts > 0 {
				calls++
			}
			if row.Usage.Repaired {
				repaired++
			}
		}
		avg := "n/a"
		if calls > 0 {
			avg = digest.Seconds(ms / int64(calls))
		}
		scored := len(r.Report.Rows) - m.Failed
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %d | %d |\n",
			r.Model, gigabytes(r.SizeBytes), digest.Seconds(r.LoadMS), avg,
			fraction(m.TP, m.TP+m.FP), fraction(m.TP, m.TP+m.FN), fraction(m.TP+m.TN, scored),
			m.Failed, repaired)
	}
	relevant := 0
	for _, row := range first.Rows {
		if row.Relevant {
			relevant++
		}
	}
	fmt.Fprintf(&b, "\n%d labeled papers (%d relevant). Every model saw the same prompt, labels, and abstracts at temperature 0. "+
		"Load is the time to load the model after unloading it from Ollama.\n", len(first.Rows), relevant)
	return b.String()
}

func fraction(num, den int) string {
	return fmt.Sprintf("%s (%d/%d)", Ratio(num, den), num, den)
}

// gigabytes formats bytes as decimal GB to one place, as `ollama list` does,
// rounding half up with integer math.
func gigabytes(b int64) string {
	tenths := (b + 50_000_000) / 100_000_000
	return fmt.Sprintf("%d.%d GB", tenths/10, tenths%10)
}
