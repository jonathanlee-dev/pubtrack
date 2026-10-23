package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const validReply = `{"summary":"S.","relevance":5,"reason":"R."}`

func fakeOllama(t *testing.T, opts Options, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts.BaseURL = srv.URL
	if opts.Model == "" {
		opts.Model = "llama3.2:3b"
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = "SYS"
	}
	if opts.Format == "" {
		opts.Format = "schema"
	}
	return New(opts)
}

func chatReply(w http.ResponseWriter, content, doneReason string, promptTokens, outputTokens int) {
	json.NewEncoder(w).Encode(map[string]any{
		"message":           map[string]string{"role": "assistant", "content": content},
		"done":              true,
		"done_reason":       doneReason,
		"prompt_eval_count": promptTokens,
		"eval_count":        outputTokens,
	})
}

func tagsHandler(version string, models ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			io.WriteString(w, `{"version":"`+version+`"}`)
		case "/api/tags":
			var list []map[string]string
			for _, m := range models {
				list = append(list, map[string]string{"name": m, "model": m})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": list})
		default:
			http.NotFound(w, r)
		}
	}
}

func TestPreflightOK(t *testing.T) {
	c := fakeOllama(t, Options{}, tagsHandler("0.6.2", "llama3.2:3b"))
	v, err := c.Preflight(context.Background())
	if err != nil || v != "0.6.2" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestPreflightUntaggedModelMeansLatest(t *testing.T) {
	c := fakeOllama(t, Options{Model: "llama3.2"}, tagsHandler("0.6.2", "llama3.2:latest"))
	if _, err := c.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightModelMissing(t *testing.T) {
	c := fakeOllama(t, Options{}, tagsHandler("0.6.2", "llama3.2:1b"))
	_, err := c.Preflight(context.Background())
	var me *ModelMissingError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want ModelMissingError", err)
	}
	if !strings.Contains(err.Error(), "ollama pull llama3.2:3b") {
		t.Errorf("message doesn't say how to fix it: %v", err)
	}
}

func TestPreflightOldVersion(t *testing.T) {
	c := fakeOllama(t, Options{}, tagsHandler("0.4.7", "llama3.2:3b"))
	_, err := c.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "0.5.0") {
		t.Fatalf("err = %v, want version error", err)
	}
}

func TestPreflightOllamaNotRunning(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	closed := srv.URL
	srv.Close()
	c := New(Options{BaseURL: closed, Model: "llama3.2:3b"})
	_, err := c.Preflight(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "ollama serve") {
		t.Errorf("message doesn't say how to fix it: %v", err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"0.5.0": true, "0.4.9": false, "0.12.3": true, "1.0.0": true,
		"0.5.0-rc1": true, "v0.6.0": true, "": false, "garbage": false,
	} {
		if got := versionAtLeast(v, 0, 5); got != want {
			t.Errorf("versionAtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestWarmupLoadsModel(t *testing.T) {
	var body map[string]any
	c := fakeOllama(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"model":"llama3.2:3b","response":"","done":true,"done_reason":"load"}`)
	})
	if _, err := c.Warmup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "llama3.2:3b" || body["stream"] != false {
		t.Errorf("warm-up request = %v", body)
	}
	if _, hasPrompt := body["prompt"]; hasPrompt {
		t.Error("warm-up should send no prompt (that's what makes Ollama just load the model)")
	}
}

type capturedChat struct {
	Model    string             `json:"model"`
	Stream   *bool              `json:"stream"`
	Format   json.RawMessage    `json:"format"`
	Options  map[string]float64 `json:"options"`
	Messages []message          `json:"messages"`
}

func TestSummarizeRequestAndUsage(t *testing.T) {
	var got capturedChat
	c := fakeOllama(t, Options{NumCtx: 4096, NumPredict: 512}, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		chatReply(w, validReply, "stop", 812, 64)
	})
	res, u, err := c.Summarize(context.Background(), Paper{PMID: "1", Title: "T", Journal: "J", Abstract: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Summary: "S.", Relevance: 5, Reason: "R."}) {
		t.Errorf("result = %+v", res)
	}
	if u.PromptTokens != 812 || u.OutputTokens != 64 || u.Attempts != 1 || u.Repaired {
		t.Errorf("usage = %+v", u)
	}

	if got.Stream == nil || *got.Stream {
		t.Error("stream must be sent as false")
	}
	if !strings.HasPrefix(string(got.Format), "{") || !strings.Contains(string(got.Format), `"required"`) {
		t.Errorf("format = %s, want the JSON schema", got.Format)
	}
	if got.Options["num_ctx"] != 4096 || got.Options["num_predict"] != 512 || got.Options["temperature"] != 0 {
		t.Errorf("options = %v", got.Options)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "SYS" || !strings.Contains(got.Messages[1].Content, "Title: T") {
		t.Errorf("messages = %+v", got.Messages)
	}
}

func TestSummarizePlainJSONMode(t *testing.T) {
	var got capturedChat
	c := fakeOllama(t, Options{Format: "json"}, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		chatReply(w, validReply, "stop", 1, 1)
	})
	if _, _, err := c.Summarize(context.Background(), Paper{Abstract: "A"}); err != nil {
		t.Fatal(err)
	}
	if string(got.Format) != `"json"` {
		t.Errorf("format = %s, want \"json\"", got.Format)
	}
}

func TestSummarizeRetriesWithFeedback(t *testing.T) {
	var calls int32
	var second capturedChat
	c := fakeOllama(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatReply(w, "Sure! This paper is about gait.", "stop", 500, 10)
			return
		}
		json.NewDecoder(r.Body).Decode(&second)
		chatReply(w, validReply, "stop", 540, 30)
	})
	_, u, err := c.Summarize(context.Background(), Paper{Abstract: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Attempts != 2 || u.PromptTokens != 1040 || u.OutputTokens != 40 {
		t.Errorf("usage = %+v", u)
	}
	// Temperature is 0, so the retry must change the conversation, not repeat it.
	if len(second.Messages) != 4 || second.Messages[2].Role != "assistant" || !strings.Contains(second.Messages[3].Content, "no JSON object") {
		t.Errorf("retry messages = %+v", second.Messages)
	}
}

func TestSummarizeGivesUpAfterTwoAttempts(t *testing.T) {
	var calls int32
	c := fakeOllama(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		chatReply(w, `{"summary":"S.","relevance":9,"reason":"R."}`, "stop", 1, 1)
	})
	_, u, err := c.Summarize(context.Background(), Paper{Abstract: "A"})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if calls != 2 || u.Attempts != 2 {
		t.Errorf("calls = %d, attempts = %d, want 2", calls, u.Attempts)
	}
}

func TestSummarizeReportsOutputCutoff(t *testing.T) {
	c := fakeOllama(t, Options{NumPredict: 512}, func(w http.ResponseWriter, r *http.Request) {
		chatReply(w, `{"summary":"S`, "length", 1, 512)
	})
	_, _, err := c.Summarize(context.Background(), Paper{Abstract: "A"})
	if err == nil || !strings.Contains(err.Error(), "num_predict=512") {
		t.Fatalf("err = %v, want it to name the num_predict cutoff", err)
	}
}

func TestSummarizeFlagsFullContext(t *testing.T) {
	c := fakeOllama(t, Options{NumCtx: 2048}, func(w http.ResponseWriter, r *http.Request) {
		chatReply(w, validReply, "stop", 2048, 30)
	})
	_, u, err := c.Summarize(context.Background(), Paper{Abstract: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if !u.ContextFull {
		t.Error("prompt filled num_ctx but ContextFull is false; Ollama may have cut the prompt")
	}
}

func TestSummarizeTruncatesLongAbstract(t *testing.T) {
	var got capturedChat
	c := fakeOllama(t, Options{MaxAbstractChars: 100}, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		chatReply(w, validReply, "stop", 1, 1)
	})
	_, u, err := c.Summarize(context.Background(), Paper{Abstract: strings.Repeat("word ", 200)})
	if err != nil {
		t.Fatal(err)
	}
	if !u.Truncated || !strings.Contains(got.Messages[1].Content, truncationMarker) {
		t.Errorf("truncated = %v; user message = %q", u.Truncated, got.Messages[1].Content)
	}
}

func TestSummarizeModelNotFound(t *testing.T) {
	c := fakeOllama(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"model \"llama3.2:3b\" not found, try pulling it first"}`)
	})
	_, _, err := c.Summarize(context.Background(), Paper{Abstract: "A"})
	var me *ModelMissingError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v, want ModelMissingError", err)
	}
}

func TestModelSize(t *testing.T) {
	c := fakeOllama(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"name":"llama3.2:1b","model":"llama3.2:1b","size":1321098329},{"name":"llama3.2:3b","model":"llama3.2:3b","size":2019393189}]}`)
	})
	size, err := c.ModelSize(context.Background())
	if err != nil || size != 2019393189 {
		t.Fatalf("got %d, %v", size, err)
	}
}

func TestUnloadSendsKeepAliveZero(t *testing.T) {
	var body map[string]any
	c := fakeOllama(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"done":true,"done_reason":"unload"}`)
	})
	if err := c.Unload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "llama3.2:3b" || body["keep_alive"] != float64(0) {
		t.Errorf("unload request = %v", body)
	}
}

func TestEmbed(t *testing.T) {
	var body map[string]any
	c := fakeOllama(t, Options{Model: "nomic-embed-text"}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"model":"nomic-embed-text","embeddings":[[0.1,0.2],[0.3,0.4]]}`)
	})
	got, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1][1] != 0.4 {
		t.Errorf("embeddings = %v", got)
	}
	if body["model"] != "nomic-embed-text" || len(body["input"].([]any)) != 2 {
		t.Errorf("request = %v", body)
	}
}

func TestEmbedCountMismatch(t *testing.T) {
	c := fakeOllama(t, Options{}, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"embeddings":[[0.1]]}`)
	})
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Error("expected an error when Ollama returns fewer embeddings than inputs")
	}
}
