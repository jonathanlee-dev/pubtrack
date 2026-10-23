// Package factcheck has a second, larger model check whether a summary says
// anything its abstract doesn't. The judge's verdict is a model's opinion,
// so reports present flags for a person to confirm, not as ground truth.
package factcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"pubtrack/internal/llm"
)

type Verdict struct {
	Supported   bool     `json:"supported"`
	Unsupported []string `json:"unsupported_claims,omitempty"`
}

// Claims come before the verdict so the model lists them before deciding.
var Schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "unsupported_claims": {"type": "array", "items": {"type": "string"}},
    "verdict": {"type": "string", "enum": ["supported", "unsupported"]}
  },
  "required": ["unsupported_claims", "verdict"]
}`)

func UserMessage(abstract, summary string) string {
	return fmt.Sprintf("Abstract:\n%s\n\nSummary:\n%s", abstract, summary)
}

// Check asks the judge whether summary is faithful to abstract.
func Check(ctx context.Context, judge *llm.Client, system, abstract, summary string) (Verdict, llm.Usage, error) {
	a, truncated := judge.Truncate(abstract)
	v, u, err := llm.Call(ctx, judge, system, UserMessage(a, summary), Schema, Validate)
	u.Truncated = truncated
	return v, u, err
}

// Validate parses a judge reply. The verdict and the claims list must agree:
// "unsupported" with no claims (or the reverse) is retried, not guessed at.
func Validate(output string) (Verdict, bool, error) {
	invalid := func(problem string) (Verdict, bool, error) {
		return Verdict{}, false, &llm.ValidationError{Problem: problem, Output: output}
	}
	fields, repaired, err := llm.ParseObject(output)
	if err != nil {
		return Verdict{}, false, err
	}

	raw, ok := fields["unsupported_claims"]
	if !ok {
		return invalid(`missing "unsupported_claims"`)
	}
	var claims []string
	if err := json.Unmarshal(raw, &claims); err != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return invalid(`"unsupported_claims" is not a list of strings`)
		}
		claims, repaired = []string{one}, true
	}
	var kept []string
	for _, c := range claims {
		if c = strings.TrimSpace(c); c != "" {
			kept = append(kept, c)
		}
	}

	verdict, problem := llm.RequiredString(fields, "verdict")
	if problem != "" {
		return invalid(problem)
	}
	if lower := strings.ToLower(verdict); lower != verdict {
		verdict, repaired = lower, true
	}
	switch {
	case verdict != "supported" && verdict != "unsupported":
		return invalid(fmt.Sprintf(`verdict %q is not "supported" or "unsupported"`, verdict))
	case verdict == "unsupported" && len(kept) == 0:
		return invalid(`verdict is "unsupported" but no claims are listed`)
	case verdict == "supported" && len(kept) > 0:
		return invalid(fmt.Sprintf(`verdict is "supported" but %d claim(s) are listed`, len(kept)))
	}
	return Verdict{Supported: verdict == "supported", Unsupported: kept}, repaired, nil
}
