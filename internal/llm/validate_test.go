package llm

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidate(t *testing.T) {
	ok := Result{Summary: "S.", Relevance: 4, Reason: "R."}
	tests := []struct {
		name     string
		in       string
		want     Result
		repaired bool
		problem  string // non-empty: expect a ValidationError containing this
	}{
		{name: "valid", in: `{"summary":"S.","relevance":4,"reason":"R."}`, want: ok},
		{name: "trailing prose", in: "Here is the JSON:\n{\"summary\":\"S.\",\"relevance\":4,\"reason\":\"R.\"}\nLet me know!", want: ok, repaired: true},
		{name: "code fence", in: "```json\n{\"summary\":\"S.\",\"relevance\":4,\"reason\":\"R.\"}\n```", want: ok, repaired: true},
		{name: "score as string", in: `{"summary":"S.","relevance":"4","reason":"R."}`, want: ok, repaired: true},
		{name: "score as float", in: `{"summary":"S.","relevance":4.0,"reason":"R."}`, want: ok, repaired: true},
		{name: "extra field ignored", in: `{"summary":"S.","relevance":4,"reason":"R.","confidence":0.9}`, want: ok},
		{name: "missing reason", in: `{"summary":"S.","relevance":4}`, problem: `missing "reason"`},
		{name: "missing relevance", in: `{"summary":"S.","reason":"R."}`, problem: `missing "relevance"`},
		{name: "out of range", in: `{"summary":"S.","relevance":7,"reason":"R."}`, problem: "outside 1–5"},
		{name: "null score", in: `{"summary":"S.","relevance":null,"reason":"R."}`, problem: "outside 1–5"},
		{name: "fractional score", in: `{"summary":"S.","relevance":4.5,"reason":"R."}`, problem: "not a whole number"},
		{name: "word score", in: `{"summary":"S.","relevance":"high","reason":"R."}`, problem: "not a number"},
		{name: "empty summary", in: `{"summary":"  ","relevance":4,"reason":"R."}`, problem: `"summary" is empty`},
		{name: "summary not a string", in: `{"summary":3,"relevance":4,"reason":"R."}`, problem: `"summary" is not a string`},
		{name: "cut off mid-object", in: `{"summary":"S.","relev`, problem: "no JSON object"},
		{name: "refusal", in: "I can't help with that.", problem: "no JSON object"},
		{name: "broken JSON in prose", in: `Sure: {"summary": "S.", relevance: 4} ok`, problem: "not valid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, repaired, err := Validate(tt.in)
			if tt.problem != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want ValidationError", err)
				}
				if !strings.Contains(ve.Problem, tt.problem) {
					t.Errorf("problem = %q, want it to contain %q", ve.Problem, tt.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if repaired != tt.repaired {
				t.Errorf("repaired = %v, want %v", repaired, tt.repaired)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got, cut := Truncate("short abstract", 100); got != "short abstract" || cut {
		t.Errorf("short input changed: %q %v", got, cut)
	}

	got, cut := Truncate(strings.Repeat("word ", 100), 50)
	if !cut || !strings.HasSuffix(got, truncationMarker) {
		t.Fatalf("got %q, %v", got, cut)
	}
	body := strings.TrimSuffix(got, truncationMarker)
	if utf8.RuneCountInString(body) > 50 {
		t.Errorf("kept %d chars, want at most 50", utf8.RuneCountInString(body))
	}
	if !strings.HasSuffix(body, "word") {
		t.Errorf("cut mid-word: %q", body)
	}

	got, _ = Truncate(strings.Repeat("ε", 100), 10)
	if !utf8.ValidString(got) {
		t.Error("truncation split a multi-byte character")
	}

	if got, cut := Truncate(strings.Repeat("x", 100), 0); cut || len(got) != 100 {
		t.Error("max 0 should disable truncation")
	}
}
