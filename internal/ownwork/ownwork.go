// Package ownwork flags papers that name the reader's own platforms.
package ownwork

import (
	"strings"
	"unicode"
)

// Matcher matches whole platform names after normalizing case and
// punctuation, so "Survey for Memory, Attention and Reaction Time" matches
// with or without the serial comma. Acronyms on their own are deliberately
// unsupported: in PubMed, "CART" is overwhelmingly CAR-T cell therapy and
// "SMART" is everything from goal-setting to trial designs.
type Matcher struct {
	names []string
	norm  []string
}

func NewMatcher(names []string) Matcher {
	var m Matcher
	for _, n := range names {
		if k := normalize(n); k != "" {
			m.names = append(m.names, n)
			m.norm = append(m.norm, k)
		}
	}
	return m
}

// Mentions returns the configured names found in text, in config order.
func (m Matcher) Mentions(text string) []string {
	t := " " + normalize(text) + " "
	var out []string
	for i, k := range m.norm {
		if strings.Contains(t, " "+k+" ") {
			out = append(out, m.names[i])
		}
	}
	return out
}

func normalize(s string) string {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.Join(words, " ")
}
