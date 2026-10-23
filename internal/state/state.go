// Package state records which PMIDs pubtrack has processed, so a re-run
// never summarizes the same paper twice.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type State struct {
	// Seen maps PMID to when it was processed: a date, "baseline", or
	// "gave-up <date>".
	Seen map[string]string `json:"seen"`
	// Failed maps PMID to failed attempts so far. Every run retries these,
	// even after they fall out of the search window.
	Failed map[string]int `json:"failed"`
	// CitationsBaselined is set once the first run has recorded the papers
	// that already cited the reader's work.
	CitationsBaselined bool `json:"citations_baselined"`
	// Keys maps a paper's DOI or normalized title to the PMID it was first
	// seen as, so a preprint's journal version, or a paper PubMed indexed
	// twice, isn't summarized again under a new PMID.
	Keys map[string]string `json:"keys,omitempty"`
}

func New() *State {
	return &State{Seen: map[string]string{}, Failed: map[string]int{}, Keys: map[string]string{}}
}

// Load reads the state file. A missing file is a first run, not an error.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	s := New()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parsing state %s: %w", path, err)
	}
	if s.Seen == nil {
		s.Seen = map[string]string{}
	}
	if s.Failed == nil {
		s.Failed = map[string]int{}
	}
	if s.Keys == nil {
		s.Keys = map[string]string{}
	}
	return s, nil
}

// Save writes to a temp file in the same directory and renames it into
// place, so an interrupted run can't leave a half-written state file.
func (s *State) Save(path string) (err error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *State) IsSeen(pmid string) bool {
	_, ok := s.Seen[pmid]
	return ok
}

func (s *State) MarkSeen(pmid, when string) {
	s.Seen[pmid] = when
	delete(s.Failed, pmid)
}

// MarkFailed records a failed attempt and returns the attempts so far.
func (s *State) MarkFailed(pmid string) int {
	s.Failed[pmid]++
	return s.Failed[pmid]
}

// KeepPending queues a paper for the next run without counting an attempt,
// for failures that weren't the paper's fault (Ollama went away mid-run).
func (s *State) KeepPending(pmid string) {
	if _, ok := s.Failed[pmid]; !ok {
		s.Failed[pmid] = 0
	}
}

// DuplicateOf returns the PMID an earlier paper with any of keys was seen
// as, if that was a different PMID.
func (s *State) DuplicateOf(pmid string, keys []string) (string, bool) {
	for _, k := range keys {
		if first, ok := s.Keys[k]; ok && first != pmid {
			return first, true
		}
	}
	return "", false
}

// RememberKeys records keys for pmid. A key already recorded keeps the PMID
// it was first seen as.
func (s *State) RememberKeys(pmid string, keys []string) {
	for _, k := range keys {
		if _, ok := s.Keys[k]; !ok {
			s.Keys[k] = pmid
		}
	}
}
