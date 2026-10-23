package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Seen) != 0 || len(s.Failed) != 0 || s.CitationsBaselined {
		t.Errorf("got %+v, want empty state", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New()
	s.MarkSeen("1", "2026-10-19")
	s.MarkFailed("2")
	s.MarkFailed("2")
	s.CitationsBaselined = true
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seen["1"] != "2026-10-19" || got.Failed["2"] != 2 || !got.CitationsBaselined {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := New()
	for i := 0; i < 2; i++ {
		if err := s.Save(path); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contains %v, want only state.json", names)
	}
}

func TestMarkSeenClearsFailure(t *testing.T) {
	s := New()
	s.MarkFailed("1")
	s.MarkSeen("1", "2026-10-19")
	if _, failed := s.Failed["1"]; failed {
		t.Error("seen paper still listed as failed")
	}
	if !s.IsSeen("1") {
		t.Error("paper not seen")
	}
}

func TestKeepPendingDoesNotCountAnAttempt(t *testing.T) {
	s := New()
	if n := s.MarkFailed("1"); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
	s.KeepPending("1")
	s.KeepPending("2")
	if s.Failed["1"] != 1 {
		t.Errorf("KeepPending changed attempt count to %d", s.Failed["1"])
	}
	if n, ok := s.Failed["2"]; !ok || n != 0 {
		t.Errorf("KeepPending on a new paper: %d, %v; want 0, true", n, ok)
	}
}

func TestDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New()
	s.RememberKeys("100", []string{"doi:10.1/a", "title:same paper"})
	s.RememberKeys("200", []string{"title:same paper"}) // already recorded: keeps 100
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if first, ok := got.DuplicateOf("300", []string{"doi:10.9/other", "title:same paper"}); !ok || first != "100" {
		t.Errorf("DuplicateOf = %q, %v; want 100, true", first, ok)
	}
	if _, ok := got.DuplicateOf("100", []string{"title:same paper"}); ok {
		t.Error("a paper was reported as a duplicate of itself")
	}
	if _, ok := got.DuplicateOf("300", []string{"title:another paper"}); ok {
		t.Error("an unseen key was reported as a duplicate")
	}
}

func TestLoadStateWithoutKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"seen":{"1":"2026-10-19"},"failed":{},"citations_baselined":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.RememberKeys("2", []string{"title:a paper"}) // must not panic on a nil map
	if s.Keys["title:a paper"] != "2" {
		t.Errorf("keys = %v", s.Keys)
	}
}
