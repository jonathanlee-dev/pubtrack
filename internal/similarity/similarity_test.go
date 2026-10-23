package similarity

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	for _, tt := range []struct {
		a, b []float64
		want float64
	}{
		{[]float64{1, 0}, []float64{1, 0}, 1},
		{[]float64{1, 0}, []float64{0, 1}, 0},
		{[]float64{1, 2}, []float64{2, 4}, 1},
		{[]float64{1, 0}, []float64{-1, 0}, -1},
		{[]float64{0, 0}, []float64{1, 0}, 0}, // zero vector: no direction, no similarity
		{[]float64{1, 0}, []float64{1}, 0},    // mismatched lengths: different models
	} {
		if got := Cosine(tt.a, tt.b); math.Abs(got-tt.want) > 1e-12 {
			t.Errorf("Cosine(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestBest(t *testing.T) {
	own := []Ref{{Label: "SMART", Vec: []float64{1, 0}}, {Label: "CART", Vec: []float64{0, 1}}}
	m := Best([]float64{0.2, 0.9}, own)
	if m.Closest != "CART" || math.Abs(m.Score-Cosine([]float64{0.2, 0.9}, []float64{0, 1})) > 1e-12 {
		t.Errorf("got %+v", m)
	}
	if got := Best([]float64{1, 0}, nil); got != (Match{}) {
		t.Errorf("no references should give no match, got %+v", got)
	}
}

func TestFixed2(t *testing.T) {
	for x, want := range map[float64]string{0.714: "0.71", 0.715: "0.72", 0.7149: "0.71", 1: "1.00", 0: "0.00", -0.25: "-0.25", -0.001: "0.00"} {
		if got := Fixed2(x); got != want {
			t.Errorf("Fixed2(%v) = %q, want %q", x, got, want)
		}
	}
}

func TestSeparation(t *testing.T) {
	scores := []Scored{
		{Score: 0.9, Relevant: true}, {Score: 0.8, Relevant: false}, {Score: 0.7, Relevant: true},
		{Score: 0.6, Relevant: false}, {Score: 0.6, Relevant: true}, {Score: 0.3, Relevant: false},
	}
	s := Separate(scores, []float64{0.5, 0.75})
	// Pairs (relevant, not): 3×3 = 9. Wins: 0.9 beats all 3; 0.7 beats 0.6 and 0.3; 0.6 beats 0.3 and ties 0.6.
	// = 3 + 2 + 1.5 = 6.5 of 9, so 13 of 18 in half-points.
	if s.AUCHalves != 13 || s.Pairs != 9 {
		t.Errorf("AUC = %d/%d half-points, want 13/18", s.AUCHalves, 2*s.Pairs)
	}
	if s.Relevant != 3 || s.NotRelevant != 3 {
		t.Errorf("counts = %d/%d", s.Relevant, s.NotRelevant)
	}
	want := []Cutoff{{Threshold: 0.5, Kept: 5, RelevantKept: 3}, {Threshold: 0.75, Kept: 2, RelevantKept: 1}}
	for i, c := range s.Cutoffs {
		if c != want[i] {
			t.Errorf("cutoff %d = %+v, want %+v", i, c, want[i])
		}
	}
	if Fixed2(s.MeanRelevant) != "0.73" || Fixed2(s.MeanNotRelevant) != "0.57" {
		t.Errorf("means = %v, %v", s.MeanRelevant, s.MeanNotRelevant)
	}
}

func TestText(t *testing.T) {
	if got := Text("T", "A"); got != "clustering: T\n\nA" {
		t.Errorf("got %q", got)
	}
}
