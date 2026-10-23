# Build notes

A running log of how this was built with a coding agent: where it helped, where it
was wrong, and what I changed. Source for the README's "How it was built" section. Record what
happened, not what should have happened.

## Process

1. Wrote the goal at a high level (project brief), then a plan: business logic, user stories, and
   the test list (`docs/PLAN.md`), reviewed before any code was generated.
2. Recorded real E-utilities responses as test fixtures before writing parsers.
3. Tests and implementation for each package were written in the same session, tests first, but
   **not compiled or run by the agent** (I run builds and tests myself). First results below.

## 2026-10-19 — planning and first implementation

Caught during planning, from live API checks rather than assumptions:

- A bare `CART[tiab]` search matched 67 papers in one week, nearly all CAR-T cell therapy. Platform
  flags therefore match full names only, never acronyms (`SMART` has the same problem).
- `elink` `pubmed_pubmed_citedin` works for the six seed papers (~150 citing papers), which made
  "cites your work" a better flag than text matching. Coverage depends on PMC full text.
- `esummary` has no abstracts and `efetch` has no JSON, so one `efetch` XML call replaced the
  planned esummary + efetch pair.

Caught while writing code:

- efetch XML includes each paper's reference list, and every reference has its own `ArticleIdList`
  with a DOI. The DOI path has to be anchored at `PubmedData>ArticleIdList`, or a cited paper's DOI
  can be reported as the paper's own. There's a test using a real record.
- `*url.Error` prints the full request URL, which carries `api_key` and `email`. Errors are
  stripped of the URL, and NCBI's 429 body (which echoes the key) is redacted.
- With `temperature: 0`, retrying an invalid model reply with the same request returns the same
  reply. The retry instead sends the bad reply back with what was wrong with it.
- Ollama silently drops the start of a prompt that overflows `num_ctx`. The only signal is
  `prompt_eval_count` reaching `num_ctx`, so that's tracked and reported per run.
- Titles and abstracts contain inline markup (`<i>`, `<sup>`); a plain chardata field drops that
  text. Parsing collects all inner text.
- esearch doesn't fail on a typo; it drops the unknown term and lists it in
  `errorlist.phrasesnotfound`. That's surfaced as a warning.

First build and run (Go 1.27.2, Ollama 0.40.6, `llama3.2:3b`, Apple Silicon):

- `go vet ./...` clean and `go test ./...` passing on the first compile. The only fix was gofmt
  alignment in one test file.
- First real run: 3 papers in the 10-day window, all summarized, 0 failures, 0 repaired outputs,
  ~1.9s per paper, 3.3s model load, 14s end to end. 133 existing citing papers recorded as baseline.
  A second run found nothing new and wrote no digest (dedupe works).

Where the agent was wrong (fill in as found):

- _Nothing caught by the build or tests yet._

Model behavior worth an eval (first run):

- The model scored the Framingham DANA paper 4/5 while its reason said the paper was "not one of
  the platforms in the reader's profile". It read "platform types" as specific named products, so
  the score and the reason contradict each other. Candidate fix for prompt v2: say the profile
  describes *kinds* of systems, not specific products.
- A dance-movement-therapy review matched the `in_home_sensing` query. The model correctly scored
  it 2/5. That's the intended split: broad queries, with the LLM doing the filtering.

## 2026-10-19 — eval set

- 30 papers sampled from eight pools (see `eval/pmids.txt` for the method). The three papers from the
  first digest were swapped out: I had already seen the model's scores for them, which could bias
  my labels.
- The labeling sheet hides which pool a paper came from and shows no model output. Order is a hash
  of the PMID, so the expected negatives are interleaved with the rest.
- Abstracts may be copyrighted by their publishers, so the labeling sheet is git-ignored and
  regenerated locally; only PMIDs and my labels are committed.

## 2026-10-19 — `eval`

- `eval` reads `eval/` (PMIDs, labels, paper cache) and prints a report pinned by a golden file.
  Smoke-tested on the three first-digest papers, not the real eval set, so the model's output on the
  labeled papers stays unseen until labeling is done.

Where the agent was wrong:

- Twice wrote a literal byte-order-mark character into source instead of the `\ufeff` escape (Go
  refused to compile; caught by `go vet`). Fixed by replacing the bytes.
- Formatted seconds with `%.1f`, which rounds half to even on the binary value (1250ms → "1.2s", not
  "1.3s"). Not hit by real data yet; caught while designing the eval report. Seconds now round half
  up with integer math, and so does the precision/recall formatting.

## 2026-10-19 — benchmark, fact-check, extraction, similarity

- `bench` runs the eval across models (llama3.2:1b, llama3.2:3b, llama3.1:8b) and writes one markdown
  table. Each model is unloaded first so load time is measured, but macOS's file cache still makes a
  recently read model load faster (2.1s vs 0.6s for the same 3B), so the load column is rough.
- `analyze` adds three things on the eval set: an 8B judge fact-checks each 3B summary against its
  abstract, structured extraction (design, n, setting, population, technology) is scored against
  hand labels, and `nomic-embed-text` similarity to my six papers is tested as a pre-filter (means,
  ranking AUC, recall/precision at cutoffs). `factcheck` runs the judge on a weekly digest.
- The weekly digest now carries extracted fields and a similarity score per paper.

Findings while building (from the 3 first-digest papers, not the labeled eval set):

- With the 3B as its own judge, it flagged all three summaries, but its "unsupported claims" were
  mostly statements the abstract does make, and once it just repeated the summary. A model that small
  can't judge its own output; that's why the judge is the 8B.
- Similarity ranked the two relevant papers above the irrelevant one, but scores bunch together: the
  dance-therapy review still scored 0.75. Whether that's good enough for a pre-filter is for the
  labeled set to show.
- Extraction over-collected technology (individual test names like "Go/No-Go") and missed it where
  the abstract was about digital technology in general.

Where the agent was wrong:

- **Schema field order.** The extraction schema was built from a map, and `json.Marshal` sorts map
  keys, so the model was forced to write `population` first and `study_design` fourth. Ollama turns
  the schema into a grammar, so field order is part of the prompt. It changed answers on 2 of 3
  papers. Fixed by building the schema in the prompt's order, with a test that pins it.
- Wrote two wrong expectations in the analysis test (a count and a cut-off title); the code was right.
- A literal byte-order mark slipped into source a third time, via a shell heredoc. The code now builds
  the character from its code point instead of writing it.

## 2026-10-20 — first real eval and benchmark

I labeled all 30 eval papers **not relevant**, including the 22 drawn from my own six queries. So the
queries (or the profile) are the weak point, and the eval can only measure false alarms for now: with
no relevant papers, recall can't be measured.

Benchmark, prompt v1, relevant = score ≥ 4:

| Model | Called relevant (of 30) | Score distribution | Avg per paper |
|---|---|---|---|
| llama3.2:1b | 29 | 5×4, 4×25, 2×1 | 0.7s |
| llama3.2:3b | 8 | 4×8, 2×17, 1×5 | 1.4s |
| llama3.1:8b | 5 | 5×1, 4×4, 3×13, 2×8, 1×4 | 3.2s |

- The 1B scores nearly everything 4 or 5. It's no use as a filter.
- The 3B never uses 3 (or 5). All 8 of its false alarms have reasons saying "adjacent" or "different
  technology", which is the rubric's 3, scored as 4. The Framingham contradiction from the first digest
  is a pattern, not a one-off. Prompt v2 should target it directly.
- The 8B uses the whole scale and has the fewest false alarms. At ~7 papers a week, its 3.2s per paper
  costs nothing that matters.
- The eval set contains the same paper twice under two PMIDs (41278633 and 42688514, "Bridging
  Language Markers and Pathology"). PubMed sometimes indexes a paper twice; the tracker should dedupe
  by DOI or normalized title.
