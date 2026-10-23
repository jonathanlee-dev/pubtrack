package llm

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const truncationMarker = " …[abstract truncated]"

type ValidationError struct {
	Problem string
	Output  string
}

func (e *ValidationError) Error() string { return "invalid model output: " + e.Problem }

// Validate parses the model's reply leniently and checks it strictly.
// Lenient: prose or a code fence around the object, or a score sent as "4"
// or 4.0, are accepted and reported as repaired. Strict: every field must be
// present, non-empty, and in range.
func Validate(output string) (Result, bool, error) {
	invalid := func(problem string) (Result, bool, error) {
		return Result{}, false, &ValidationError{Problem: problem, Output: output}
	}
	fields, repaired, err := ParseObject(output)
	if err != nil {
		return Result{}, false, err
	}

	summary, problem := requiredString(fields, "summary")
	if problem != "" {
		return invalid(problem)
	}
	relevance, fixed, problem := relevanceField(fields)
	if problem != "" {
		return invalid(problem)
	}
	reason, problem := requiredString(fields, "reason")
	if problem != "" {
		return invalid(problem)
	}
	return Result{Summary: summary, Relevance: relevance, Reason: reason}, repaired || fixed, nil
}

// ParseObject parses a reply as a JSON object, leniently: small models often
// wrap the object in prose or a code fence, which is stripped and reported
// as repaired.
func ParseObject(output string) (map[string]json.RawMessage, bool, error) {
	invalid := func(problem string) (map[string]json.RawMessage, bool, error) {
		return nil, false, &ValidationError{Problem: problem, Output: output}
	}
	s := strings.TrimSpace(output)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &fields); err == nil {
		return fields, false, nil
	}
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return invalid("no JSON object in reply")
	}
	if err := json.Unmarshal([]byte(s[i:j+1]), &fields); err != nil {
		return invalid("reply is not valid JSON")
	}
	return fields, true, nil
}

// RequiredString returns a trimmed, non-empty string field, or a problem.
func RequiredString(fields map[string]json.RawMessage, key string) (string, string) {
	return requiredString(fields, key)
}

func requiredString(fields map[string]json.RawMessage, key string) (string, string) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Sprintf("missing %q", key)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Sprintf("%q is not a string", key)
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", fmt.Sprintf("%q is empty", key)
	}
	return v, ""
}

func relevanceField(fields map[string]json.RawMessage) (n int, repaired bool, problem string) {
	raw, ok := fields["relevance"]
	if !ok {
		return 0, false, `missing "relevance"`
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f != math.Trunc(f) {
			return 0, false, fmt.Sprintf("relevance %v is not a whole number", f)
		}
		n = int(f)
		repaired = strings.ContainsAny(string(raw), ".eE")
	} else {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, false, `"relevance" is not a number`
		}
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return 0, false, fmt.Sprintf("relevance %q is not a number", s)
		}
		n, repaired = v, true
	}
	if n < 1 || n > 5 {
		return 0, false, fmt.Sprintf("relevance %d is outside 1–5", n)
	}
	return n, repaired, ""
}

// Truncate shortens an abstract to at most max characters, at a word
// boundary when one is reasonably close. max <= 0 disables truncation.
func Truncate(s string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s, false
	}
	cut := string([]rune(s)[:max])
	if i := strings.LastIndexAny(cut, " \n"); i > len(cut)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \n") + truncationMarker, true
}
