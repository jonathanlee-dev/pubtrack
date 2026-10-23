package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"pubtrack/internal/digest"
	"pubtrack/internal/state"
)

// The end-to-end tests run the real `pubtrack run` command, from config file
// to digest and state on disk, against local fakes of NCBI and Ollama.
//
// The fake world: the query matches 1001 and 1002. The reader's own paper
// (9001) is cited by 1002 and 1003; 1003 is outside the query window, so the
// first run records it as a baseline instead of putting it in the digest.

const e2eEfetch = `<?xml version="1.0" ?>
<PubmedArticleSet>
<PubmedArticle><MedlineCitation><PMID>1001</PMID><Article>
<Journal><Title>Journal of Gait Studies</Title></Journal>
<ArticleTitle>Gait speed measured at home with the Life Laboratory platform.</ArticleTitle>
<Abstract><AbstractText>Placeholder abstract about in-home gait monitoring.</AbstractText></Abstract>
<PublicationTypeList><PublicationType>Journal Article</PublicationType></PublicationTypeList>
</Article></MedlineCitation></PubmedArticle>
<PubmedArticle><MedlineCitation><PMID>1002</PMID><Article>
<Journal><Title>Aging Research</Title></Journal>
<ArticleTitle>Walking and memory in older adults.</ArticleTitle>
<Abstract><AbstractText>Placeholder abstract about walking and memory.</AbstractText></Abstract>
<PublicationTypeList><PublicationType>Journal Article</PublicationType></PublicationTypeList>
</Article></MedlineCitation></PubmedArticle>
<PubmedArticle><MedlineCitation><PMID>9001</PMID><Article>
<Journal><Title>Own Journal</Title></Journal>
<ArticleTitle>The reader's own gait paper.</ArticleTitle>
<Abstract><AbstractText>Placeholder abstract for the reader's own paper.</AbstractText></Abstract>
<PublicationTypeList><PublicationType>Journal Article</PublicationType></PublicationTypeList>
</Article></MedlineCitation></PubmedArticle>
</PubmedArticleSet>`

// fakeNCBI serves esearch, elink, and efetch for the fake world above.
func fakeNCBI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("email") == "" {
			http.Error(w, "missing email", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/esearch.fcgi":
			fmt.Fprint(w, `{"esearchresult":{"count":"2","idlist":["1001","1002"]}}`)
		case "/elink.fcgi":
			fmt.Fprint(w, `{"linksets":[{"ids":["9001"],"linksetdbs":[{"linkname":"pubmed_pubmed_citedin","links":["1002","1003"]}]}]}`)
		case "/efetch.fcgi":
			fmt.Fprint(w, e2eEfetch)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOllama answers like Ollama with the summary and embedding models
// pulled, and counts /api/chat calls.
func fakeOllama(t *testing.T, chats *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.6.0"}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"llama3.2:3b"},{"name":"nomic-embed-text:latest"}]}`)
		case "/api/generate":
			fmt.Fprint(w, `{"done":true}`)
		case "/api/chat":
			atomic.AddInt32(chats, 1)
			// The extraction schema is the only one with study_design.
			content := `{"summary":"A study of gait at home.","relevance":4,"reason":"Matches in-home sensing."}`
			if bytes.Contains(body, []byte("study_design")) {
				content = `{"study_design":"observational","sample_size":40,"setting":"home","population":"older adults","technology":["gait sensors"]}`
			}
			json.NewEncoder(w).Encode(map[string]any{
				"message":           map[string]string{"role": "assistant", "content": content},
				"done_reason":       "stop",
				"prompt_eval_count": 300,
				"eval_count":        40,
			})
		case "/api/embed":
			var req struct {
				Input []string `json:"input"`
			}
			json.Unmarshal(body, &req)
			vecs := make([][]float64, len(req.Input))
			for i := range vecs {
				vecs[i] = []float64{1, 0.5}
			}
			json.NewEncoder(w).Encode(map[string]any{"embeddings": vecs})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeE2EConfig writes a config pointing at the fakes. state_path and
// output_dir are relative, so they resolve next to the config file.
func writeE2EConfig(t *testing.T, ncbi, ollama string) string {
	t.Helper()
	cfg := map[string]any{
		"email":           "test@example.com",
		"ncbi_base_url":   ncbi,
		"profile":         "Researcher interested in in-home gait monitoring.",
		"queries":         []map[string]string{{"name": "gait", "term": "gait[tiab]"}},
		"own_papers":      []map[string]string{{"pmid": "9001", "label": "Own gait paper"}},
		"platform_names":  []string{"Life Laboratory"},
		"extract_fields":  true,
		"embedding_model": "nomic-embed-text",
		"ollama":          map[string]any{"url": ollama},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = realMain(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestEndToEndRun(t *testing.T) {
	var chats int32
	configPath := writeE2EConfig(t, fakeNCBI(t).URL, fakeOllama(t, &chats).URL)
	dir := filepath.Dir(configPath)

	code, _, stderr := runCLI(t, "run", "-config", configPath)
	if code != 0 {
		t.Fatalf("first run: exit %d, stderr:\n%s", code, stderr)
	}

	digests, err := filepath.Glob(filepath.Join(dir, "digests", "digest-*.md"))
	if err != nil || len(digests) != 1 {
		t.Fatalf("digests written: %v (err %v), want exactly one", digests, err)
	}
	md, err := os.ReadFile(digests[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Gait speed measured at home with the Life Laboratory platform.",
		"Walking and memory in older adults.",
		"Cites your work",
		"Own gait paper",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("digest markdown is missing %q", want)
		}
	}
	if strings.Contains(string(md), "The reader's own gait paper.") {
		t.Error("the reader's own paper was listed in the digest")
	}

	data, err := os.ReadFile(strings.TrimSuffix(digests[0], ".md") + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var d digest.Digest
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("digest JSON: %v", err)
	}
	if len(d.Items) != 2 {
		t.Fatalf("digest has %d items, want 2 (1001 and 1002)", len(d.Items))
	}
	for _, it := range d.Items {
		id := it.Article.PMID
		if it.Error != "" {
			t.Errorf("%s: error %q", id, it.Error)
		}
		if it.Result == nil || it.Result.Relevance != 4 {
			t.Errorf("%s: result = %+v, want relevance 4", id, it.Result)
		}
		if it.Extraction == nil || it.Extraction.StudyDesign != "observational" {
			t.Errorf("%s: extraction = %+v", id, it.Extraction)
		}
		if it.Similarity == nil || it.Similarity.Closest != "Own gait paper" {
			t.Errorf("%s: similarity = %+v", id, it.Similarity)
		}
	}
	byID := map[string]digest.Item{}
	for _, it := range d.Items {
		byID[it.Article.PMID] = it
	}
	if got := byID["1001"].Mentions; !slices.Equal(got, []string{"Life Laboratory"}) {
		t.Errorf("1001 platform mentions = %v", got)
	}
	if got := byID["1002"].Cites; !slices.Equal(got, []string{"Own gait paper"}) {
		t.Errorf("1002 cites = %v", got)
	}
	if d.Stats.Baselined != 1 {
		t.Errorf("baselined = %d, want 1 (1003)", d.Stats.Baselined)
	}

	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1001", "1002", "1003"} {
		if !st.IsSeen(id) {
			t.Errorf("state: %s not marked seen", id)
		}
	}
	if st.Seen["1003"] != "baseline" {
		t.Errorf("state: 1003 = %q, want baseline", st.Seen["1003"])
	}
	if !st.CitationsBaselined {
		t.Error("state: citations not marked baselined")
	}
	if st.IsSeen("9001") {
		t.Error("state: the reader's own paper was marked seen")
	}

	// A second run finds nothing new: no digest, and no model calls.
	before := atomic.LoadInt32(&chats)
	code, _, stderr = runCLI(t, "run", "-config", configPath)
	if code != 0 {
		t.Fatalf("second run: exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "no new papers; no digest written") {
		t.Errorf("second run stderr:\n%s", stderr)
	}
	if after := atomic.LoadInt32(&chats); after != before {
		t.Errorf("second run made %d chat calls, want 0", after-before)
	}
	if again, _ := filepath.Glob(filepath.Join(dir, "digests", "digest-*.md")); len(again) != 1 {
		t.Errorf("digests after second run: %v, want still one", again)
	}
}

func TestEndToEndDryRunWritesNothing(t *testing.T) {
	var chats int32
	configPath := writeE2EConfig(t, fakeNCBI(t).URL, fakeOllama(t, &chats).URL)
	dir := filepath.Dir(configPath)

	code, stdout, stderr := runCLI(t, "run", "-config", configPath, "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.HasPrefix(stdout, "# pubtrack digest") || !strings.Contains(stdout, "Walking and memory in older adults.") {
		t.Errorf("dry-run stdout is not the digest:\n%s", stdout)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("dry run wrote %s", e.Name())
		}
	}
}

func TestEndToEndOllamaDownFailsBeforeNCBI(t *testing.T) {
	var ncbiCalls int32
	ncbi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ncbiCalls, 1)
	}))
	t.Cleanup(ncbi.Close)
	// A closed server's address refuses connections, like Ollama not running.
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	configPath := writeE2EConfig(t, ncbi.URL, down.URL)
	code, _, stderr := runCLI(t, "run", "-config", configPath)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "can't reach Ollama") {
		t.Errorf("stderr doesn't say Ollama is down:\n%s", stderr)
	}
	if n := atomic.LoadInt32(&ncbiCalls); n != 0 {
		t.Errorf("made %d NCBI requests with Ollama down, want 0", n)
	}
}
