# pubtrack

[![test](https://github.com/jonathanlee-dev/pubtrack/actions/workflows/test.yml/badge.svg)](https://github.com/jonathanlee-dev/pubtrack/actions/workflows/test.yml)

Watches PubMed for new papers matching a set of queries, has a **local LLM** (Ollama) summarize each
one and score its relevance against a reader profile, and writes a markdown + JSON digest. Run it
from cron.

The model runs on your machine: no API key, no per-call cost, and nothing leaves the machine except
the PubMed queries. It's also a small, measured study of what a 2–5 GB local model can and can't do
as a literature filter. See [Evals](#evals).

## Install

Download an archive for your platform from [Releases](https://github.com/jonathanlee-dev/pubtrack/releases)
and unpack it. It holds the `pubtrack` binary, this README, and `config.example.json`; the prompts
are built into the binary.

The macOS binaries aren't signed by Apple, so macOS blocks them at first. Clear the download flag
once:

```sh
xattr -d com.apple.quarantine ./pubtrack
```

Or build from source with Go 1.26 or later: `go build ./cmd/pubtrack`.

You also need [Ollama](https://ollama.com) 0.5 or later.

## Quickstart

```sh
ollama pull llama3.2:3b                 # ~2 GB
cp config.example.json config.json      # then set "email" (NCBI asks clients to identify themselves)

pubtrack check                          # config, Ollama, and how many papers each query finds
pubtrack run -dry-run                   # full run, digest to stdout, nothing written
pubtrack run                            # writes digests/digest-YYYY-MM-DD.{md,json} and state.json
pubtrack eval                           # scores the model against eval/labels.csv
pubtrack eval -model llama3.2:1b -prompt v3    # compare a model or prompt
pubtrack bench                          # eval across llama3.2:1b, llama3.2:3b, llama3.1:8b in one table
pubtrack analyze                        # fact-check summaries (8B judge), score extraction, test similarity
pubtrack factcheck -digest digests/digest-YYYY-MM-DD.json
```

From a source checkout, `go run ./cmd/pubtrack` stands in for `pubtrack`. `eval`, `bench`, and
`analyze` read the eval set in `eval/`, which comes with the source, not the release archives.

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
6. Skip papers already seen under another PMID, matched by DOI or title: a preprint's journal
   version, or a paper PubMed indexed twice.
7. Rule-based flags: which queries matched, whether it cites your work, whether it names your platforms.
8. Summarize and score each paper one at a time with a JSON schema; validate; retry once with feedback.
9. Write the digest, then save state.

On the first run, papers that already cited your work are recorded as seen instead of filling the
first digest.

## Design decisions

- **The model only does what code can't.** It writes the summary, the 1–5 relevance score, and a
  one-sentence reason. Which query matched, whether a paper cites your work, and whether it names
  your platforms are all decided by code. Matching is on full platform names only: an acronym
  search for `CART` matched 67 papers in one week, nearly all about CAR-T cell therapy.
- **Search by Entrez date, not publication date.** Entrez date is when a paper entered PubMed, so
  papers indexed late or published online ahead of print aren't missed. A 10-day window on a weekly
  run overlaps by design; the state file removes the overlap.
- **Constrained, validated output.** Each request sends a JSON schema, which Ollama turns into a
  grammar the model must follow. Replies are still validated (generation can stop mid-object), and
  a bad reply is retried once with the reply and what was wrong with it. Resending the same request
  would be pointless at temperature 0.
- **Schema field order is part of the prompt.** The grammar makes the model write fields in schema
  order, and reordering the extraction schema changed answers on 2 of 3 test papers. The schema is
  built in the prompt's order, and a test pins it.
- **One request at a time.** A local model serves one request at a time; sending more only queues
  them and invites timeouts.
- **State is saved last, atomically.** The digest is written first; if that fails, the same papers
  come back next run. The state file is written to a temporary file and renamed into place.
- **Standard library only.** No third-party Go dependencies.

## Failure handling

| Failure | What happens |
|---|---|
| Ollama not running, too old, or model not pulled | One clear line saying what to do; exits non-zero before any NCBI request |
| Model cold after idle | A warm-up request loads it first, with a longer timeout than normal requests |
| Invalid model output | Retried once with feedback; then listed as "not summarized" and retried next run, up to `max_attempts` (3) |
| Ollama stops responding mid-run | Remaining papers are kept for next run without counting an attempt |
| NCBI rate limit or server error | Exponential backoff (or `Retry-After`), up to 4 retries; requests stay under 3/s (10/s with an API key) |
| One query fails | Warning in the digest; the run continues. Every query failing is an error |
| PMID missing from `efetch` | That paper fails; the others continue |
| No abstract | Listed, but not scored: scoring a title alone invites the model to make things up |
| Erratum | Skipped |
| Prompt fills the context window | Flagged in the run summary (Ollama silently drops the start of an over-long prompt) |
| Embedding model missing | Similarity is turned off with a warning; the digest still runs |

## Evals

The eval set is 30 papers sampled from eight pools (see `eval/pmids.txt`), labeled relevant or not
before any model output was seen. The threshold, a score of 4 or 5, was fixed before labeling.

**All 30 were labeled not relevant**, including the 22 drawn from the tracker's own queries. That
says the queries are broader than the profile. It also means the eval can measure false alarms but
not recall, so it can't yet say which model misses fewer relevant papers.

Prompt v1, temperature 0, on an Apple silicon Mac:

| Model | Size | False alarms (of 30) | Avg per paper |
|---|---|---|---|
| llama3.2:1b | 1.3 GB | 29 | 0.7s |
| llama3.2:3b (default) | 2.0 GB | 8 | 1.4s |
| llama3.1:8b | 4.9 GB | 5 | 3.2s |

- The 1B scores nearly everything 4 or 5, so it's no use as a filter.
- The 3B never used a score of 3. All 8 of its false alarms gave a reason that described the paper as
  "adjacent" or using "different technology", which is the rubric's 3, and scored it 4.
- Two follow-up prompts aimed at that pattern made things worse: v2 raised false alarms to 17 (3B)
  and 11 (8B); v3 cost the 3B both of two papers judged relevant. Both are kept in `prompts/` with
  their results in `eval/results/`. Tuning further against the same 30 papers would only weaken the
  eval as a test.
- The default stays the 3B with v1: it caught both relevant papers in the first digest, where the 8B
  scored one of them 3. The digest lists every paper sorted by score, so a false alarm costs a few
  seconds of reading.
- A 3B model can't fact-check its own summaries: as its own judge it flagged statements its
  abstracts do make. `analyze` and `factcheck` use the 8B as the judge.

## Limitations

- Citation links come from PMC full text, so "Cites your work" is partial and lags.
- The eval set has no relevant papers yet (see above), and the judgments on relevant papers rest on
  two examples.
- Duplicate detection covers papers processed since it was added; older state has no titles or DOIs.
- Release binaries aren't code-signed.

## How it was built

Built with a coding agent, starting from a written plan, user stories, and a test list
([docs/PLAN.md](docs/PLAN.md)) reviewed before any code was written. Real E-utilities responses were
recorded as test fixtures before the parsers were written. [docs/BUILD_NOTES.md](docs/BUILD_NOTES.md)
logs what happened, including where the agent was wrong: a map-ordered JSON schema that silently
changed the model's answers, byte-order marks written into source, a helper rewritten into a call to
itself, and a prompt revision that doubled false alarms. Each was caught by a test, the eval, or
review.

## Development

```sh
go vet ./...
go test -race ./...
```

All tests run offline. Network clients are tested against local `httptest` servers with recorded
NCBI responses, and end-to-end tests run the real `pubtrack run` command against local fakes of
NCBI and Ollama. CI runs gofmt, vet, and the tests on the oldest supported Go release and the
newest. Pushing a `v*` tag builds and publishes release archives.

## License

MIT
