// Package extract pulls study details (design, sample size, setting,
// population, technology) out of an abstract as structured fields, and scores
// those fields against hand labels.
package extract

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"pubtrack/internal/llm"
)

var (
	Designs  = []string{"randomized controlled trial", "observational", "feasibility or pilot", "validation", "qualitative", "review", "protocol", "other", "not stated"}
	Settings = []string{"home", "community", "clinic or hospital", "long-term care", "laboratory", "online", "other", "not stated"}
)

type Fields struct {
	StudyDesign string `json:"study_design"`
	// SampleSize is 0 when the abstract doesn't state it.
	SampleSize int      `json:"sample_size"`
	Setting    string   `json:"setting"`
	Population string   `json:"population"`
	Technology []string `json:"technology"`
}

// Schema constrains the reply; the enums come from Designs and Settings so
// the schema, the validator, and the label checks can't drift apart.
//
// Property order matters: Ollama compiles the schema into a grammar that makes
// the model write fields in this order, and order changes the answers. It
// matches the order the prompt describes. (Built by hand because marshaling a
// Go map sorts the keys alphabetically.)
var Schema = func() json.RawMessage {
	designs, err := json.Marshal(Designs)
	if err != nil {
		panic(err)
	}
	settings, err := json.Marshal(Settings)
	if err != nil {
		panic(err)
	}
	return json.RawMessage(`{"type":"object","properties":{` +
		`"study_design":{"type":"string","enum":` + string(designs) + `},` +
		`"sample_size":{"type":"integer","minimum":0},` +
		`"setting":{"type":"string","enum":` + string(settings) + `},` +
		`"population":{"type":"string"},` +
		`"technology":{"type":"array","items":{"type":"string"}}},` +
		`"required":["study_design","sample_size","setting","population","technology"]}`)
}()

// Run extracts fields from one paper.
func Run(ctx context.Context, c *llm.Client, system string, p llm.Paper) (Fields, llm.Usage, error) {
	abstract, truncated := c.Truncate(p.Abstract)
	f, u, err := llm.Call(ctx, c, system, llm.UserMessage(p.Title, p.Journal, abstract), Schema, Validate)
	u.Truncated = truncated
	return f, u, err
}

// Validate parses an extraction reply, accepting near misses (enum casing, a
// number sent as a string, one technology sent as a string) as repaired.
func Validate(output string) (Fields, bool, error) {
	invalid := func(problem string) (Fields, bool, error) {
		return Fields{}, false, &llm.ValidationError{Problem: problem, Output: output}
	}
	fields, repaired, err := llm.ParseObject(output)
	if err != nil {
		return Fields{}, false, err
	}
	var f Fields
	var fixed bool
	var problem string

	if f.StudyDesign, fixed, problem = enumField(fields, "study_design", Designs); problem != "" {
		return invalid(problem)
	}
	repaired = repaired || fixed
	if f.SampleSize, fixed, problem = sizeField(fields); problem != "" {
		return invalid(problem)
	}
	repaired = repaired || fixed
	if f.Setting, fixed, problem = enumField(fields, "setting", Settings); problem != "" {
		return invalid(problem)
	}
	repaired = repaired || fixed
	if f.Population, problem = llm.RequiredString(fields, "population"); problem != "" {
		return invalid(problem)
	}
	if f.Technology, fixed, problem = listField(fields, "technology"); problem != "" {
		return invalid(problem)
	}
	return f, repaired || fixed, nil
}

func enumField(fields map[string]json.RawMessage, key string, allowed []string) (string, bool, string) {
	raw, problem := llm.RequiredString(fields, key)
	if problem != "" {
		return "", false, problem
	}
	v := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	if !slices.Contains(allowed, v) {
		return "", false, fmt.Sprintf("%s %q is not one of: %s", key, raw, strings.Join(allowed, ", "))
	}
	return v, v != raw, ""
}

func sizeField(fields map[string]json.RawMessage) (int, bool, string) {
	raw, ok := fields["sample_size"]
	if !ok {
		return 0, false, `missing "sample_size"`
	}
	if string(raw) == "null" {
		return 0, true, ""
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f < 0 || f != math.Trunc(f) {
			return 0, false, fmt.Sprintf("sample_size %v is not a whole number of participants", f)
		}
		return int(f), false, ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 {
			return n, true, ""
		}
	}
	return 0, false, `"sample_size" is not a whole number`
}

func listField(fields map[string]json.RawMessage, key string) ([]string, bool, string) {
	raw, ok := fields[key]
	if !ok {
		return nil, false, fmt.Sprintf("missing %q", key)
	}
	var list []string
	repaired := false
	if err := json.Unmarshal(raw, &list); err != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return nil, false, fmt.Sprintf("%q is not a list of strings", key)
		}
		list, repaired = []string{one}, true
	}
	out := []string{}
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out, repaired, ""
}

// Line renders fields as one line for the digest.
func Line(f Fields) string {
	size := "n not stated"
	if f.SampleSize > 0 {
		size = fmt.Sprintf("n = %d", f.SampleSize)
	}
	tech := "none stated"
	if len(f.Technology) > 0 {
		tech = strings.Join(f.Technology, ", ")
	}
	return fmt.Sprintf("Design: %s · %s · Setting: %s · Population: %s · Technology: %s", f.StudyDesign, size, f.Setting, f.Population, tech)
}

// Label is a hand label for the fields that can be scored exactly. An empty
// field (nil SampleSize) wasn't labeled.
type Label struct {
	StudyDesign string
	SampleSize  *int
	Setting     string
}

// ReadLabels reads extraction_labels.csv (pmid, study_design, sample_size,
// setting columns). A missing file means no labels yet.
func ReadLabels(path string) (map[string]Label, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Label{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]Label{}
	if len(records) == 0 {
		return out, nil
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, string(rune(0xFEFF)))))] = i
	}
	for i, rec := range records[1:] {
		get := func(name string) string {
			j, ok := col[name]
			if !ok || j >= len(rec) {
				return ""
			}
			return strings.ToLower(strings.Join(strings.Fields(rec[j]), " "))
		}
		pmid := get("pmid")
		if pmid == "" {
			continue
		}
		var l Label
		if v := get("study_design"); v != "" {
			if !slices.Contains(Designs, v) {
				return nil, fmt.Errorf("%s line %d: study_design %q is not one of: %s", path, i+2, v, strings.Join(Designs, ", "))
			}
			l.StudyDesign = v
		}
		if v := get("sample_size"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s line %d: sample_size %q is not a whole number (0 = not stated)", path, i+2, v)
			}
			l.SampleSize = &n
		}
		if v := get("setting"); v != "" {
			if !slices.Contains(Settings, v) {
				return nil, fmt.Errorf("%s line %d: setting %q is not one of: %s", path, i+2, v, strings.Join(Settings, ", "))
			}
			l.Setting = v
		}
		if l.StudyDesign != "" || l.SampleSize != nil || l.Setting != "" {
			out[pmid] = l
		}
	}
	return out, nil
}

type Tally struct{ Correct, Labeled int }

type Mismatch struct {
	PMID, Field, Labeled, Extracted string
}

type Scores struct {
	Design, Size, Setting Tally
	Mismatches            []Mismatch
}

// Score compares extracted fields with labels, field by field, in the order
// of pmids. A paper with a label but no extraction (it failed) isn't scored.
func Score(pmids []string, got map[string]Fields, labels map[string]Label) Scores {
	var s Scores
	check := func(t *Tally, pmid, field, want, have string) {
		t.Labeled++
		if want == have {
			t.Correct++
			return
		}
		s.Mismatches = append(s.Mismatches, Mismatch{PMID: pmid, Field: field, Labeled: want, Extracted: have})
	}
	for _, pmid := range pmids {
		l, ok := labels[pmid]
		f, extracted := got[pmid]
		if !ok || !extracted {
			continue
		}
		if l.StudyDesign != "" {
			check(&s.Design, pmid, "design", l.StudyDesign, f.StudyDesign)
		}
		if l.SampleSize != nil {
			check(&s.Size, pmid, "sample size", strconv.Itoa(*l.SampleSize), strconv.Itoa(f.SampleSize))
		}
		if l.Setting != "" {
			check(&s.Setting, pmid, "setting", l.Setting, f.Setting)
		}
	}
	return s
}
