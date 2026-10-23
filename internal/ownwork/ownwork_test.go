package ownwork

import (
	"reflect"
	"testing"
)

func TestMentions(t *testing.T) {
	m := NewMatcher([]string{
		"ORCATECH",
		"Collaborative Aging Research Using Technology",
		"Survey for Memory, Attention, and Reaction Time",
		"I-CONECT",
	})
	tests := []struct {
		text string
		want []string
	}{
		{"Data came from the Survey for Memory, Attention and Reaction Time.", []string{"Survey for Memory, Attention, and Reaction Time"}},
		{"We used ORCATECH in-home sensors.", []string{"ORCATECH"}},
		{"Built on the Collaborative Aging Research Using Technology (CART) initiative", []string{"Collaborative Aging Research Using Technology"}},
		{"Secondary analysis of the I CONECT trial", []string{"I-CONECT"}},
		// The reason acronyms are never matched on their own:
		{"CAR-T cell therapy (CART) in relapsed lymphoma", nil},
		{"A SMART goal-setting app for weight loss", nil},
		{"orcatechnology is not a word", nil},
	}
	for _, tt := range tests {
		if got := m.Mentions(tt.text); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Mentions(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}
