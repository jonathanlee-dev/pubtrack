// Package similarity scores how close a paper is to the reader's own papers,
// using embeddings from a local model (nomic-embed-text). With a few hundred
// papers a week, a plain loop over cosine similarity is enough: no vector
// database or index.
package similarity

import (
	"fmt"
	"math"
)

// Text is what gets embedded for a paper. nomic-embed-text expects a task
// prefix; "clustering" is the one for comparing documents with each other
// (as opposed to "search_query"/"search_document" for asymmetric search).
func Text(title, abstract string) string {
	return "clustering: " + title + "\n\n" + abstract
}

// Cosine similarity of two vectors; 0 for a zero vector or mismatched lengths.
func Cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Ref is one of the reader's own papers.
type Ref struct {
	Label string
	Vec   []float64
}

type Match struct {
	Score   float64 `json:"score"`
	Closest string  `json:"closest"`
}

// Best is the highest similarity to any of the reader's papers.
func Best(v []float64, own []Ref) Match {
	var m Match
	for i, r := range own {
		if s := Cosine(v, r.Vec); i == 0 || s > m.Score {
			m = Match{Score: s, Closest: r.Label}
		}
	}
	return m
}

// Fixed2 formats a score to two places, rounding half away from zero.
func Fixed2(x float64) string {
	v := int64(math.Round(math.Abs(x) * 100))
	sign := ""
	if x < 0 && v != 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// Scored is one labeled paper's similarity score.
type Scored struct {
	Score    float64
	Relevant bool
}

type Cutoff struct {
	Threshold    float64
	Kept         int
	RelevantKept int
}

// Separation says how well similarity alone tells relevant papers apart,
// which is what it would need to do to work as a cheap pre-filter.
type Separation struct {
	Relevant, NotRelevant         int
	MeanRelevant, MeanNotRelevant float64
	// AUC is the chance a random relevant paper outscores a random
	// irrelevant one, counted in half-points (a tie is one half-point, a
	// win two) so it's an exact fraction: AUCHalves / (2 * Pairs).
	AUCHalves, Pairs int
	Cutoffs          []Cutoff
}

func Separate(scores []Scored, thresholds []float64) Separation {
	var s Separation
	var sumR, sumN float64
	for _, x := range scores {
		if x.Relevant {
			s.Relevant++
			sumR += x.Score
		} else {
			s.NotRelevant++
			sumN += x.Score
		}
	}
	if s.Relevant > 0 {
		s.MeanRelevant = sumR / float64(s.Relevant)
	}
	if s.NotRelevant > 0 {
		s.MeanNotRelevant = sumN / float64(s.NotRelevant)
	}
	for _, r := range scores {
		if !r.Relevant {
			continue
		}
		for _, n := range scores {
			if n.Relevant {
				continue
			}
			s.Pairs++
			switch {
			case r.Score > n.Score:
				s.AUCHalves += 2
			case r.Score == n.Score:
				s.AUCHalves++
			}
		}
	}
	for _, t := range thresholds {
		c := Cutoff{Threshold: t}
		for _, x := range scores {
			if x.Score >= t {
				c.Kept++
				if x.Relevant {
					c.RelevantKept++
				}
			}
		}
		s.Cutoffs = append(s.Cutoffs, c)
	}
	return s
}
