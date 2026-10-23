package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `{"email":"a@example.org","profile":"P","queries":[{"name":"q","term":"t"}]}`

// The shipped example must always load, so a fresh clone works.
func TestExampleConfigIsValid(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Queries) != 6 || len(c.OwnPapers) != 6 || c.Ollama.Model != "llama3.2:3b" {
		t.Errorf("unexpected example config: %d queries, %d own papers, model %q", len(c.Queries), len(c.OwnPapers), c.Ollama.Model)
	}
	if !filepath.IsAbs(c.StatePath) || !filepath.IsAbs(c.OutputDir) {
		t.Errorf("paths not resolved: %q %q", c.StatePath, c.OutputDir)
	}
}

func TestDefaultsAndRelativePaths(t *testing.T) {
	path := writeConfig(t, minimal)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.WindowDays != 10 || c.MaxAttempts != 3 || c.PromptVersion != "v1" || c.Tool != "pubtrack" {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.Ollama.URL != "http://localhost:11434" || c.Ollama.Model != "llama3.2:3b" || c.Ollama.Format != "schema" || c.Ollama.NumCtx != 4096 {
		t.Errorf("ollama defaults not applied: %+v", c.Ollama)
	}
	// Relative to the config file, not the working directory (cron runs from $HOME).
	if want := filepath.Join(filepath.Dir(path), "state.json"); c.StatePath != want {
		t.Errorf("state path = %q, want %q", c.StatePath, want)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := map[string]struct{ body, want string }{
		"missing email":    {`{"profile":"P","queries":[{"name":"q","term":"t"}]}`, "email"},
		"no queries":       {`{"email":"a@example.org","profile":"P"}`, "queries"},
		"missing profile":  {`{"email":"a@example.org","queries":[{"name":"q","term":"t"}]}`, "profile"},
		"duplicate query":  {`{"email":"a@example.org","profile":"P","queries":[{"name":"q","term":"t"},{"name":"q","term":"u"}]}`, "duplicate"},
		"bad pmid":         {`{"email":"a@example.org","profile":"P","queries":[{"name":"q","term":"t"}],"own_papers":[{"pmid":"PMC123"}]}`, "PubMed ID"},
		"bad format":       {`{"email":"a@example.org","profile":"P","queries":[{"name":"q","term":"t"}],"ollama":{"format":"yaml"}}`, "format"},
		"typo in key name": {`{"emial":"a@example.org","profile":"P","queries":[{"name":"q","term":"t"}]}`, "emial"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestDefaultOwnPaperLabel(t *testing.T) {
	c, err := Load(writeConfig(t, `{"email":"a@example.org","profile":"P","queries":[{"name":"q","term":"t"}],"own_papers":[{"pmid":"123"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.OwnPapers[0].Label != "PMID 123" {
		t.Errorf("label = %q", c.OwnPapers[0].Label)
	}
}
