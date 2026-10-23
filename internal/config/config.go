// Package config loads pubtrack's JSON config file, fills in defaults, and
// validates it before anything touches the network.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Query struct {
	Name string `json:"name"`
	Term string `json:"term"`
}

// OwnPaper is one of the reader's own papers; new papers citing it are flagged.
type OwnPaper struct {
	PMID  string `json:"pmid"`
	Label string `json:"label"`
}

type Ollama struct {
	URL   string `json:"url"`
	Model string `json:"model"`
	// Format is "schema" (send a JSON schema; Ollama 0.5+) or "json" (plain JSON mode).
	Format                string `json:"format"`
	NumCtx                int    `json:"num_ctx"`
	NumPredict            int    `json:"num_predict"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
	LoadTimeoutSeconds    int    `json:"load_timeout_seconds"`
}

type Config struct {
	Email         string     `json:"email"`
	NCBIAPIKey    string     `json:"ncbi_api_key"`
	Tool          string     `json:"tool"`
	WindowDays    int        `json:"window_days"`
	Queries       []Query    `json:"queries"`
	OwnPapers     []OwnPaper `json:"own_papers"`
	PlatformNames []string   `json:"platform_names"`
	Profile       string     `json:"profile"`
	PromptVersion string     `json:"prompt_version"`
	// ExtractFields adds structured study details (design, n, setting…) to each paper.
	ExtractFields        bool   `json:"extract_fields"`
	ExtractPromptVersion string `json:"extract_prompt_version"`
	// EmbeddingModel scores each paper's similarity to the reader's own papers.
	// Empty turns similarity off.
	EmbeddingModel   string `json:"embedding_model"`
	MaxAbstractChars int    `json:"max_abstract_chars"`
	MaxAttempts      int    `json:"max_attempts"`
	StatePath        string `json:"state_path"`
	OutputDir        string `json:"output_dir"`
	Ollama           Ollama `json:"ollama"`
}

var pmidPattern = regexp.MustCompile(`^[0-9]+$`)

// Load reads and validates the config at path. Relative state_path and
// output_dir resolve against the config file's directory, so a cron job
// doesn't depend on its working directory.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	defer f.Close()

	var c Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // a misspelled key should fail, not silently use a default
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c.resolvePaths(filepath.Dir(path))
	return c, nil
}

func (c *Config) applyDefaults() {
	setString(&c.Tool, "pubtrack")
	setInt(&c.WindowDays, 10)
	setString(&c.PromptVersion, "v1")
	setString(&c.ExtractPromptVersion, "extract-v1")
	setInt(&c.MaxAbstractChars, 6000)
	setInt(&c.MaxAttempts, 3)
	setString(&c.StatePath, "state.json")
	setString(&c.OutputDir, "digests")
	setString(&c.Ollama.URL, "http://localhost:11434")
	setString(&c.Ollama.Model, "llama3.2:3b")
	setString(&c.Ollama.Format, "schema")
	setInt(&c.Ollama.NumCtx, 4096)
	setInt(&c.Ollama.NumPredict, 512)
	setInt(&c.Ollama.RequestTimeoutSeconds, 120)
	setInt(&c.Ollama.LoadTimeoutSeconds, 300)
	for i := range c.OwnPapers {
		setString(&c.OwnPapers[i].Label, "PMID "+c.OwnPapers[i].PMID)
	}
}

func (c *Config) validate() error {
	var errs []error
	if !strings.Contains(c.Email, "@") {
		errs = append(errs, errors.New(`"email" is required: NCBI asks every E-utilities client to identify itself`))
	}
	if len(c.Queries) == 0 {
		errs = append(errs, errors.New(`at least one entry in "queries" is required`))
	}
	names := map[string]bool{}
	for i, q := range c.Queries {
		if strings.TrimSpace(q.Name) == "" || strings.TrimSpace(q.Term) == "" {
			errs = append(errs, fmt.Errorf("queries[%d]: name and term are both required", i))
		}
		if names[q.Name] {
			errs = append(errs, fmt.Errorf("queries[%d]: duplicate name %q", i, q.Name))
		}
		names[q.Name] = true
	}
	for i, p := range c.OwnPapers {
		if !pmidPattern.MatchString(p.PMID) {
			errs = append(errs, fmt.Errorf("own_papers[%d]: %q is not a PubMed ID", i, p.PMID))
		}
	}
	if strings.TrimSpace(c.Profile) == "" {
		errs = append(errs, errors.New(`"profile" is required: it is what the model scores relevance against`))
	}
	if c.WindowDays < 1 || c.WindowDays > 365 {
		errs = append(errs, fmt.Errorf("window_days must be 1–365, got %d", c.WindowDays))
	}
	if c.Ollama.Format != "schema" && c.Ollama.Format != "json" {
		errs = append(errs, fmt.Errorf(`ollama.format must be "schema" or "json", got %q`, c.Ollama.Format))
	}
	return errors.Join(errs...)
}

func (c *Config) resolvePaths(dir string) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for _, p := range []*string{&c.StatePath, &c.OutputDir} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
}

func setString(p *string, v string) {
	if strings.TrimSpace(*p) == "" {
		*p = v
	}
}

func setInt(p *int, v int) {
	if *p == 0 {
		*p = v
	}
}
