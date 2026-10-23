# pubtrack

Watches PubMed for new papers matching a set of queries, has a **local LLM** (Ollama) summarize each
one and score its relevance against a reader profile, and writes a markdown + JSON digest. Run it
from cron.

## Quickstart

Requirements: Go 1.22+, [Ollama](https://ollama.com) 0.5 or later.

```sh
ollama pull llama3.2:3b                 # ~2 GB
cp config.example.json config.json      # then set "email" (NCBI asks clients to identify themselves)

go test ./...
go run ./cmd/pubtrack check             # config, Ollama, and how many papers each query finds
go run ./cmd/pubtrack run -dry-run      # full run, digest to stdout, nothing written
go run ./cmd/pubtrack run               # writes digests/digest-YYYY-MM-DD.{md,json} and state.json
go run ./cmd/pubtrack eval              # scores the model against eval/labels.csv
go run ./cmd/pubtrack eval -model llama3.2:1b -prompt v2    # compare a model or prompt
go run ./cmd/pubtrack bench             # eval across llama3.2:1b, llama3.2:3b, llama3.1:8b in one table
go run ./cmd/pubtrack analyze           # fact-check summaries (8B judge), score extraction, test similarity
go run ./cmd/pubtrack factcheck -digest digests/digest-YYYY-MM-DD.json
```

With `extract_fields` and `embedding_model` set (they are in the example config), each digest entry
also gets study details (design, n, setting, population, technology) and a similarity score against
your own papers. `analyze` and `factcheck` use `llama3.1:8b` as the judge and `nomic-embed-text` for
embeddings: `ollama pull llama3.1:8b nomic-embed-text`.

Weekly from cron (paths in the config resolve relative to the config file, so the working
directory doesn't matter):

```cron
0 7 * * 1  /path/to/pubtrack run -config /path/to/config.json >> /path/to/pubtrack.log 2>&1
```

## How a run works

1. Check Ollama is up, new enough, and has the model; load the model (a cold load can take a while).
2. `esearch` each query over the last `window_days` by **Entrez date** (when the paper entered PubMed).
3. One `elink` call finds papers that cite the reader's own papers.
4. Drop PMIDs already in `state.json`; add earlier failures for a retry.
5. `efetch` the rest as XML, in batches of 100.
6. Rule-based flags: which queries matched, whether it cites your work, whether it names your platforms.
7. Summarize and score each paper one at a time with a JSON schema; validate; retry once with feedback.
8. Write the digest, then save state.

On the first run, papers that already cited your work are recorded as seen instead of filling the
first digest.

## License

MIT
