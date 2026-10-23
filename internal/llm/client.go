// Package llm calls a local Ollama server to summarize a paper and score its
// relevance, and validates what comes back.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "http://localhost:11434"
	keepAlive      = "10m"
	maxAttempts    = 2
)

// ErrUnavailable means Ollama couldn't be reached at all. Callers stop
// sending work rather than failing every remaining paper one by one.
var ErrUnavailable = errors.New("ollama unavailable")

type unavailableError struct {
	baseURL string
	err     error
}

func (e *unavailableError) Error() string {
	return fmt.Sprintf("can't reach Ollama at %s (%v). Is it running? Start it with `ollama serve` or open the Ollama app", e.baseURL, e.err)
}
func (e *unavailableError) Is(target error) bool { return target == ErrUnavailable }
func (e *unavailableError) Unwrap() error        { return e.err }

type ModelMissingError struct{ Model string }

func (e *ModelMissingError) Error() string {
	return fmt.Sprintf("model %q isn't available in Ollama. Pull it with: ollama pull %s", e.Model, e.Model)
}

type StatusError struct {
	Path       string
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Ollama %s: HTTP %d: %s", e.Path, e.StatusCode, e.Message)
}

type Options struct {
	BaseURL          string
	Model            string
	SystemPrompt     string
	Format           string // "schema" or "json"
	NumCtx           int
	NumPredict       int
	MaxAbstractChars int
	RequestTimeout   time.Duration
	// LoadTimeout applies to the warm-up call only. Loading a model after
	// idle can take far longer than answering a prompt once it's loaded.
	LoadTimeout time.Duration
}

type Client struct {
	opt      Options
	http     *http.Client
	loadHTTP *http.Client
}

func New(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	o.BaseURL = strings.TrimSuffix(o.BaseURL, "/")
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 2 * time.Minute
	}
	if o.LoadTimeout <= 0 {
		o.LoadTimeout = 5 * time.Minute
	}
	return &Client{
		opt:      o,
		http:     &http.Client{Timeout: o.RequestTimeout},
		loadHTTP: &http.Client{Timeout: o.LoadTimeout},
	}
}

// Preflight checks that Ollama is reachable, new enough for JSON-schema
// output, and has the model pulled. It returns the Ollama version.
func (c *Client) Preflight(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, c.http, http.MethodGet, "/api/version", nil, &v); err != nil {
		return "", err
	}
	if !versionAtLeast(v.Version, 0, 5) {
		return v.Version, fmt.Errorf("Ollama version %q: pubtrack needs 0.5.0 or later for JSON-schema output. Update Ollama", v.Version)
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := c.do(ctx, c.http, http.MethodGet, "/api/tags", nil, &tags); err != nil {
		return v.Version, err
	}
	want := tagged(c.opt.Model)
	for _, m := range tags.Models {
		if m.Name == want || m.Model == want {
			return v.Version, nil
		}
	}
	return v.Version, &ModelMissingError{Model: c.opt.Model}
}

// ModelSize returns the model's size on disk in bytes, as Ollama reports it.
func (c *Client) ModelSize(ctx context.Context) (int64, error) {
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			Size  int64  `json:"size"`
		} `json:"models"`
	}
	if err := c.do(ctx, c.http, http.MethodGet, "/api/tags", nil, &tags); err != nil {
		return 0, err
	}
	want := tagged(c.opt.Model)
	for _, m := range tags.Models {
		if m.Name == want || m.Model == want {
			return m.Size, nil
		}
	}
	return 0, &ModelMissingError{Model: c.opt.Model}
}

// Unload drops the model from Ollama's memory (keep_alive 0), so the next
// Warmup measures a real load instead of a model that's still resident.
func (c *Client) Unload(ctx context.Context) error {
	return c.do(ctx, c.http, http.MethodPost, "/api/generate", map[string]any{
		"model":      c.opt.Model,
		"stream":     false,
		"keep_alive": 0,
	}, nil)
}

// tagged adds the implicit ":latest" tag Ollama uses for an untagged name.
func tagged(model string) string {
	if strings.Contains(model, ":") {
		return model
	}
	return model + ":latest"
}

// Warmup loads the model into memory. A generate request with no prompt
// only loads the model, so the first real request doesn't time out.
func (c *Client) Warmup(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	err := c.do(ctx, c.loadHTTP, http.MethodPost, "/api/generate", map[string]any{
		"model":      c.opt.Model,
		"stream":     false,
		"keep_alive": keepAlive,
	}, nil)
	return time.Since(start), err
}

// Summarize asks the model for a summary, relevance score, and reason. Calls
// are made one at a time by the caller: a local model serves one request at
// a time, so fanning out only queues requests and risks timeouts.
func (c *Client) Summarize(ctx context.Context, p Paper) (Result, Usage, error) {
	abstract, truncated := c.Truncate(p.Abstract)
	res, u, err := Call(ctx, c, c.opt.SystemPrompt, UserMessage(p.Title, p.Journal, abstract), resultSchema, Validate)
	u.Truncated = truncated
	return res, u, err
}

// Model is the Ollama model this client calls.
func (c *Client) Model() string { return c.opt.Model }

// Truncate shortens text to the configured max_abstract_chars.
func (c *Client) Truncate(s string) (string, bool) { return Truncate(s, c.opt.MaxAbstractChars) }

// Validator parses and checks a model reply, reporting whether it had to be
// repaired. It returns a *ValidationError for a reply worth retrying.
type Validator[T any] func(output string) (T, bool, error)

// Call sends one structured request: the reply is constrained to schema and
// must pass validate. An unusable reply is retried once. Temperature is 0, so
// resending the same request would return the same reply; the retry shows the
// model its answer and what was wrong with it instead.
func Call[T any](ctx context.Context, c *Client, system, user string, schema json.RawMessage, validate Validator[T]) (T, Usage, error) {
	var zero T
	var u Usage
	messages := []message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		u.Attempts = attempt
		var resp chatResponse
		start := time.Now()
		err := c.do(ctx, c.http, http.MethodPost, "/api/chat", c.chatRequest(messages, schema), &resp)
		u.LatencyMS += time.Since(start).Milliseconds()
		if err != nil {
			return zero, u, err
		}
		u.PromptTokens += resp.PromptEvalCount
		u.OutputTokens += resp.EvalCount
		// Ollama drops the start of a prompt that overflows num_ctx without
		// saying so; a prompt that fills the window is the only sign.
		if c.opt.NumCtx > 0 && resp.PromptEvalCount >= c.opt.NumCtx {
			u.ContextFull = true
		}

		v, repaired, err := validate(resp.Message.Content)
		if err == nil {
			u.Repaired = repaired
			return v, u, nil
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			return zero, u, err
		}
		if resp.DoneReason == "length" {
			err = fmt.Errorf("%w (output stopped at num_predict=%d tokens)", err, c.opt.NumPredict)
		}
		lastErr = err
		messages = append(messages[:2:2],
			message{Role: "assistant", Content: resp.Message.Content},
			message{Role: "user", Content: "That reply could not be used: " + problemOf(err) + ". Reply again with only the JSON object."},
		)
	}
	return zero, u, lastErr
}

// Embed returns one embedding per input, in a single request to an
// embedding model such as nomic-embed-text.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float64, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	var resp struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := c.do(ctx, c.loadHTTP, http.MethodPost, "/api/embed", map[string]any{
		"model":      c.opt.Model,
		"input":      inputs,
		"keep_alive": keepAlive,
	}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("Ollama /api/embed returned %d embeddings for %d inputs", len(resp.Embeddings), len(inputs))
	}
	return resp.Embeddings, nil
}

func (c *Client) chatRequest(messages []message, schema json.RawMessage) chatRequest {
	opts := map[string]any{"temperature": 0}
	if c.opt.NumCtx > 0 {
		opts["num_ctx"] = c.opt.NumCtx
	}
	if c.opt.NumPredict > 0 {
		opts["num_predict"] = c.opt.NumPredict
	}
	var format any = schema
	if c.opt.Format == "json" {
		format = "json"
	}
	return chatRequest{
		Model:     c.opt.Model,
		Messages:  messages,
		Stream:    false,
		Format:    format,
		Options:   opts,
		KeepAlive: keepAlive,
	}
}

func problemOf(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Problem
	}
	return err.Error()
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.opt.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return &unavailableError{baseURL: c.opt.BaseURL, err: op.Err}
		}
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return fmt.Errorf("Ollama %s timed out after %s. A slow machine may need a larger request_timeout_seconds", path, hc.Timeout)
		}
		return fmt.Errorf("Ollama %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("Ollama %s: reading response: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		if resp.StatusCode == http.StatusNotFound && strings.Contains(msg, "model") && strings.Contains(msg, "not found") {
			return &ModelMissingError{Model: c.opt.Model}
		}
		return &StatusError{Path: path, StatusCode: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("Ollama %s: decoding response: %w", path, err)
	}
	return nil
}

// versionAtLeast compares major.minor of an Ollama version like "0.6.2" or
// "0.5.0-rc1". Anything unparseable fails the check.
func versionAtLeast(v string, major, minor int) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}
