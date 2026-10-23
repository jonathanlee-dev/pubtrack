package extract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pubtrack/internal/llm"
)

func TestValidate(t *testing.T) {
	full := Fields{StudyDesign: "observational", SampleSize: 186, Setting: "home", Population: "adults 75+ with MCI", Technology: []string{"wearable accelerometer"}}
	accepted := []struct {
		name, in string
		want     Fields
		repaired bool
	}{
		{"valid", `{"study_design":"observational","sample_size":186,"setting":"home","population":"adults 75+ with MCI","technology":["wearable accelerometer"]}`, full, false},
		{"enum casing", `{"study_design":"Observational","sample_size":186,"setting":" Home ","population":"adults 75+ with MCI","technology":["wearable accelerometer"]}`, full, true},
		{"size as string", `{"study_design":"observational","sample_size":"186","setting":"home","population":"adults 75+ with MCI","technology":["wearable accelerometer"]}`, full, true},
		{"size null means not stated", `{"study_design":"review","sample_size":null,"setting":"not stated","population":"older adults","technology":[]}`,
			Fields{StudyDesign: "review", Setting: "not stated", Population: "older adults", Technology: []string{}}, true},
		{"technology as a string", `{"study_design":"observational","sample_size":186,"setting":"home","population":"adults 75+ with MCI","technology":"wearable accelerometer"}`, full, true},
	}
	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			got, repaired, err := Validate(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) || repaired != tt.repaired {
				t.Errorf("got %+v repaired=%v, want %+v repaired=%v", got, repaired, tt.want, tt.repaired)
			}
		})
	}

	rejected := []struct{ name, in, problem string }{
		{"unknown design", `{"study_design":"cohort","sample_size":1,"setting":"home","population":"p","technology":[]}`, `study_design "cohort"`},
		{"unknown setting", `{"study_design":"review","sample_size":1,"setting":"moon","population":"p","technology":[]}`, `setting "moon"`},
		{"negative size", `{"study_design":"review","sample_size":-3,"setting":"home","population":"p","technology":[]}`, "sample_size"},
		{"fractional size", `{"study_design":"review","sample_size":2.5,"setting":"home","population":"p","technology":[]}`, "sample_size"},
		{"missing population", `{"study_design":"review","sample_size":1,"setting":"home","technology":[]}`, `missing "population"`},
		{"missing technology", `{"study_design":"review","sample_size":1,"setting":"home","population":"p"}`, `missing "technology"`},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Validate(tt.in)
			var ve *llm.ValidationError
			if !errors.As(err, &ve) || !strings.Contains(ve.Problem, tt.problem) {
				t.Errorf("err = %v, want problem containing %q", err, tt.problem)
			}
		})
	}
}

// The grammar Ollama builds from the schema fixes the order the model writes
// fields in, and that order changes its answers.
func TestSchemaKeepsPromptFieldOrder(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(Schema, &parsed); err != nil {
		t.Fatalf("schema isn't valid JSON: %v", err)
	}
	s := string(Schema)
	last := -1
	for _, key := range []string{`"study_design":{`, `"sample_size":{`, `"setting":{`, `"population":{`, `"technology":{`} {
		i := strings.Index(s, key)
		if i < last {
			t.Errorf("%s is out of order in %s", key, s)
		}
		last = i
	}
}

func TestSchemaListsEveryAllowedValue(t *testing.T) {
	for _, v := range append(append([]string{}, Designs...), Settings...) {
		if !strings.Contains(string(Schema), `"`+v+`"`) {
			t.Errorf("schema is missing %q", v)
		}
	}
}

func TestReadLabels(t *testing.T) {
	p := filepath.Join(t.TempDir(), "extraction_labels.csv")
	csv := "n,pmid,study_design,sample_size,setting,note,title\n" +
		"1,100,observational,186,home,,\"A, B\"\n" +
		"2,200,,,,,Unlabeled\n" +
		"3,300,Review,0,,,Only some fields\n"
	if err := os.WriteFile(p, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLabels(p)
	if err != nil {
		t.Fatal(err)
	}
	n186, n0 := 186, 0
	want := map[string]Label{
		"100": {StudyDesign: "observational", SampleSize: &n186, Setting: "home"},
		"300": {StudyDesign: "review", SampleSize: &n0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	if err := os.WriteFile(p, []byte("pmid,study_design\n100,cohort\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLabels(p); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("err = %v, want a line-numbered error for an unknown design", err)
	}

	if got, err := ReadLabels(filepath.Join(t.TempDir(), "missing.csv")); err != nil || len(got) != 0 {
		t.Errorf("missing file should be no labels, got %v, %v", got, err)
	}
}

func TestScore(t *testing.T) {
	n186, n50 := 186, 50
	labels := map[string]Label{
		"1": {StudyDesign: "observational", SampleSize: &n186, Setting: "home"},
		"2": {StudyDesign: "review", SampleSize: &n50},
	}
	got := map[string]Fields{
		"1": {StudyDesign: "observational", SampleSize: 186, Setting: "community"},
		"2": {StudyDesign: "review", SampleSize: 49, Setting: "home"},
	}
	s := Score([]string{"1", "2"}, got, labels)
	if s.Design != (Tally{Correct: 2, Labeled: 2}) || s.Size != (Tally{Correct: 1, Labeled: 2}) || s.Setting != (Tally{Correct: 0, Labeled: 1}) {
		t.Errorf("score = %+v", s)
	}
	want := []Mismatch{
		{PMID: "1", Field: "setting", Labeled: "home", Extracted: "community"},
		{PMID: "2", Field: "sample size", Labeled: "50", Extracted: "49"},
	}
	if !reflect.DeepEqual(s.Mismatches, want) {
		t.Errorf("mismatches = %+v", s.Mismatches)
	}
}

func TestLine(t *testing.T) {
	f := Fields{StudyDesign: "observational", SampleSize: 186, Setting: "home", Population: "adults 75+", Technology: []string{"accelerometer", "app"}}
	if got := Line(f); got != "Design: observational · n = 186 · Setting: home · Population: adults 75+ · Technology: accelerometer, app" {
		t.Errorf("got %q", got)
	}
	if got := Line(Fields{StudyDesign: "review", Setting: "not stated", Population: "older adults"}); got != "Design: review · n not stated · Setting: not stated · Population: older adults · Technology: none stated" {
		t.Errorf("got %q", got)
	}
}
