package llm

import (
	"encoding/json"
	"fmt"
)

type Paper struct {
	PMID     string
	Title    string
	Journal  string
	Abstract string
}

// Result is the only thing the model is asked for. Topics and own-work flags
// are known from the query and citation data, so they are not delegated.
type Result struct {
	Summary   string `json:"summary"`
	Relevance int    `json:"relevance"`
	Reason    string `json:"reason"`
}

// Usage is recorded for every paper so each digest can report tokens,
// latency, and how often the model's output needed help.
type Usage struct {
	PromptTokens int   `json:"prompt_tokens"`
	OutputTokens int   `json:"output_tokens"`
	LatencyMS    int64 `json:"latency_ms"`
	Attempts     int   `json:"attempts"`
	Truncated    bool  `json:"truncated,omitempty"`
	Repaired     bool  `json:"repaired,omitempty"`
	ContextFull  bool  `json:"context_full,omitempty"`
}

// resultSchema is sent as Ollama's "format" (0.5+), which constrains
// decoding to this shape. Output is still validated: generation can stop at
// num_predict mid-object, and the schema can't say a summary must be useful.
var resultSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary":   {"type": "string"},
    "relevance": {"type": "integer", "minimum": 1, "maximum": 5},
    "reason":    {"type": "string"}
  },
  "required": ["summary", "relevance", "reason"]
}`)

func UserMessage(title, journal, abstract string) string {
	return fmt.Sprintf("Title: %s\nJournal: %s\n\nAbstract:\n%s", title, journal, abstract)
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string         `json:"model"`
	Messages  []message      `json:"messages"`
	Stream    bool           `json:"stream"`
	Format    any            `json:"format,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
	KeepAlive string         `json:"keep_alive,omitempty"`
}

type chatResponse struct {
	Message         message `json:"message"`
	DoneReason      string  `json:"done_reason"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
}
