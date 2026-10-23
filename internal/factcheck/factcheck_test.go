package factcheck

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"pubtrack/internal/llm"
)

func TestValidate(t *testing.T) {
	accepted := []struct {
		name, in string
		want     Verdict
		repaired bool
	}{
		{"supported", `{"unsupported_claims":[],"verdict":"supported"}`, Verdict{Supported: true}, false},
		{"unsupported", `{"unsupported_claims":["n=200 is not in the abstract"],"verdict":"unsupported"}`, Verdict{Unsupported: []string{"n=200 is not in the abstract"}}, false},
		{"verdict casing", `{"unsupported_claims":[],"verdict":"Supported"}`, Verdict{Supported: true}, true},
		{"claim as a string", `{"unsupported_claims":"claims a cure","verdict":"unsupported"}`, Verdict{Unsupported: []string{"claims a cure"}}, true},
		{"blank claims dropped", `{"unsupported_claims":["  ", "x"],"verdict":"unsupported"}`, Verdict{Unsupported: []string{"x"}}, false},
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
		{"unknown verdict", `{"unsupported_claims":[],"verdict":"mostly"}`, "verdict"},
		{"missing verdict", `{"unsupported_claims":[]}`, `missing "verdict"`},
		{"missing claims", `{"verdict":"supported"}`, `missing "unsupported_claims"`},
		{"unsupported with no claims", `{"unsupported_claims":[],"verdict":"unsupported"}`, "no claims"},
		{"supported with claims", `{"unsupported_claims":["x"],"verdict":"supported"}`, "1 claim"},
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

func TestUserMessage(t *testing.T) {
	if got := UserMessage("A.", "S."); got != "Abstract:\nA.\n\nSummary:\nS." {
		t.Errorf("got %q", got)
	}
}
