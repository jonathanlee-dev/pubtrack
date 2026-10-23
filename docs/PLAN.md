# pubtrack — plan (business logic, user stories, tests)

Follows `Publication_tracker_project_brief.md`. Written 2026-10-19. Moves into the repo as
`docs/PLAN.md` once the repo exists. Review this before any code is written.

## Decisions (2026-10-19)

| Decision | Choice |
|---|---|
| Repo / license | `pubtrack`, MIT |
| Phase 1 (ship) | Core CLI + eval set + token/latency tracking + prompt comparison |
| Phase 1.5 | OpenTelemetry tracing, then one hosted-model comparison run |
| Later | MCP server, digest to colleagues, React viewer, `tsnet` |
| Default model | `llama3.2:3b` (~2 GB). `llama3.2:1b` is run through the eval as a measured comparison |
| E-utilities calls | `esearch` → `efetch` (XML, batched). No `esummary`: efetch XML has everything |
| Date filter | `datetype=edat`, 10-day window on a weekly run (overlap is deduped by state) |
| Config format | JSON (standard library) |
| Score scale | Integer 1–5; "relevant" = 4 or 5. Threshold fixed **before** labeling |
| Own-work flag | Yes: citation lookup + full-name mention match (below) |

**Still needed:** repo location on disk, GitHub username (for the module path).

## How a run works

1. Load config and state (seen PMIDs, failed PMIDs with attempt count).
2. **Preflight:** check Ollama is up (`GET /api/tags`) and the model is pulled. Fail fast with a
   one-line message before spending any NCBI calls.
3. For each query: `esearch` over the date window → PMIDs. Record which query matched each PMID.
4. One `elink` call (`pubmed_pubmed_citedin`, all six own PMIDs) → set of citing PMIDs. New ones are
   added to the run even if no query matched.
5. Drop PMIDs already seen. Retry previously failed ones (up to 3 attempts total).
6. `efetch` the rest in batches (XML) → title, journal, authors, date, DOI, abstract.
   A PMID requested but missing from the response is a per-paper failure, not a run failure.
7. Rule-based flags (no LLM): `cites_own_work`, `mentions_own_platform`, matched queries.
8. For each paper, **sequentially**: truncate abstract if needed → call Ollama with the JSON schema
   → validate → on failure retry once → record tokens in/out and latency.
9. Write `digest-YYYY-MM-DD.md` and `digest-YYYY-MM-DD.json`; update state last, atomically
   (write temp file, rename).

The LLM is only asked for what code can't know: summary, relevance score, reason. Topics come from
which query matched; own-work flags are deterministic.

**First run:** the ~150 papers that already cite your work are recorded as seen without summarizing
(baseline), so the first digest isn't 150 entries. *Confirm this, or prefer one big first digest?*

## Queries (PubMed syntax, measured 2026-10-19)

| Name | Seeded by | Last 365 days |
|---|---|---|
| `in_home_sensing` | JMIR Form Res 2020, J Vis Exp 2018 | 45 |
| `digital_biomarkers` | Digit Biomark 2020 (CART) | 187 |
| `remote_cognitive_assessment` | Gerontology 2021 (SMART) | 78 |
| `walking_reminiscence` | Gerontologist 2025 | 46 |
| `conversational_engagement` | Gerontologist 2024 (I-CONECT) | 17 |
| `own_platforms` | all | 4 |

```
in_home_sensing:
("in-home"[tiab] OR "home-based"[tiab] OR ambient[tiab] OR passive[tiab] OR unobtrusive[tiab])
AND (sensor*[tiab] OR "remote monitoring"[tiab]) AND ("Aged"[Mesh] OR "older adults"[tiab])
AND ("Cognitive Dysfunction"[Mesh] OR "mild cognitive impairment"[tiab] OR dementia[tiab] OR cognit*[tiab])

digital_biomarkers:
("digital biomarker*"[tiab] OR "digital phenotyp*"[tiab]) AND (aging[tiab] OR "older adults"[tiab]
OR dementia[tiab] OR "Alzheimer Disease"[Mesh] OR "mild cognitive impairment"[tiab])

remote_cognitive_assessment:
(unsupervised[tiab] OR "self-administered"[tiab] OR "web-based"[tiab] OR remote[tiab] OR smartphone[tiab])
AND ("cognitive assessment"[tiab] OR "cognitive test*"[tiab] OR "neuropsychological test*"[tiab])
AND ("older adults"[tiab] OR "Aged"[Mesh])

walking_reminiscence:
(reminiscence[tiab] OR "walking group*"[tiab] OR "walking intervention*"[tiab])
AND ("older adults"[tiab] OR "Aged"[Mesh]) AND (cognit*[tiab] OR "social engagement"[tiab] OR "brain health"[tiab])

conversational_engagement:
("social isolation"[tiab] OR loneliness[tiab] OR "social engagement"[tiab])
AND (conversation*[tiab] OR "video chat"[tiab] OR videoconferenc*[tiab] OR "internet-based"[tiab])
AND (cognit*[tiab] OR "mild cognitive impairment"[tiab]) AND ("older adults"[tiab] OR "Aged"[Mesh])

own_platforms:
(ORCATECH[tiab] OR "Oregon Center for Aging and Technology"[tiab]
OR "Collaborative Aging Research Using Technology"[tiab]
OR "Survey for Memory, Attention, and Reaction Time"[tiab] OR "I-CONECT"[tiab])
```

`digital_biomarkers` is the broadest and will pull in some non-aging work; that's what the relevance
score is for, and the eval set will show whether it filters correctly.

## Own-work flags

- **Cites your work** — `elink` `pubmed_pubmed_citedin` on:
  40974151 (Gerontologist 2025), 37935416 (I-CONECT 2024), 33827088 (SMART 2021),
  33442584 (CART 2020), 32310138 (veterans 2020), 30102277 (life lab 2018).
  Limitation (README): citation links come from PMC full text, so coverage is partial and lags.
- **Mentions your platform** — case-sensitive match on title + abstract for the full names in
  `own_platforms`. **Never bare "SMART" or "CART"**: `CART[tiab]` matched 67 papers in one week,
  nearly all CAR-T cell therapy. This is a good README example of why the flag is rule-based.

## Relevance profile (sent to the model)

> I am a research engineer who built and operated technology platforms for aging research:
> in-home sensor networks that passively monitor older adults' activity, sleep, and mobility to
> detect cognitive decline; a self-administered web-based cognitive assessment for older adults;
> a shared research platform for collecting continuous remote health data across studies; a
> GPS-guided walking app for a social reminiscence intervention; and the technology side of an
> internet-based conversational engagement trial for socially isolated older adults. I want
> papers about how these kinds of systems are built, validated, deployed, or used to measure
> cognition and daily function in older adults.

**Rubric (in the prompt):**

| Score | Meaning |
|---|---|
| 5 | Directly about one of these platform types, used with older adults for cognition/function |
| 4 | Same area and population, but the technology is secondary or only partly matches |
| 3 | Adjacent: right technology, different population or outcome (or the reverse) |
| 2 | Keyword overlap only |
| 1 | Unrelated |

**Output schema:** `{ "summary": string (≤3 plain-language sentences), "relevance": integer 1–5,
"reason": string (one sentence) }`. Validation also checks ranges and non-empty strings.

## Eval set

- 30 papers stored as local fixtures (abstract text included) so evals never hit NCBI.
- ~20 sampled across the six queries from the last year; ~10 clear negatives (CAR-T papers from
  `CART[tiab]`, plus older-adult papers from unrelated fields).
- **Each paper is labeled relevant or not relevant before any model output is seen.**
- `pubtrack eval` reports the confusion matrix with raw counts, precision, and recall, per
  model × prompt version. Prompts live as versioned files (`prompts/v1.txt` …).

## User stories

1. When the weekly run finishes, a markdown digest lists the new papers sorted by relevance,
   each with a summary, the model's reason, matched topics, and a PubMed/DOI link.
2. A re-run in the same week summarizes no paper twice.
3. When a new paper cites one of the reader's own papers, it appears in a "Cites your work" section
   even if no query matched it.
4. When a paper names one of the reader's platforms in full, it is flagged; a CAR-T paper is not.
5. When Ollama isn't running or the model isn't pulled, the run prints one clear line saying what
   to do and exits non-zero before calling NCBI.
6. When the model is cold, the first call doesn't time out.
7. When one paper's LLM output is still invalid after a retry, that paper appears as
   "not summarized" with its link, the run continues, and it's retried next week (max 3 attempts).
8. When NCBI returns 429 or 5xx, the client backs off and retries, and never exceeds 3 req/s
   (10 with an API key).
9. Each digest ends with a run summary: papers processed, failures, tokens in/out, latency.
10. `pubtrack eval` scores a model + prompt version against the hand labels.
11. A JSON file is written alongside the markdown, for the Phase 2 viewer.

## Tests (written before code)

| Test | Story |
|---|---|
| efetch XML fixture parses title, journal, authors, date, DOI, abstract; structured abstracts (BACKGROUND/METHODS…) joined correctly | 1 |
| PMID requested but absent from efetch response → per-paper failure | 7 |
| esearch / elink JSON fixtures parse | 1, 3 |
| Seen PMID skipped; failed PMID retried; dropped after 3 attempts | 2, 7 |
| State write is atomic (no partial file on error) | 2 |
| Platform-mention matcher: full names match, "SMART", "CART", "CAR-T" don't | 4 |
| Throttle: N requests take ≥ (N-1)/rate seconds | 8 |
| Retry: 429 and 5xx retried with backoff; 400 not retried; `Retry-After` honored | 8 |
| Ollama down (connection refused) → typed error with readable message | 5 |
| Model not in `/api/tags` → readable error | 5 |
| LLM validation: valid; trailing prose; missing field; score as string; score out of range; empty summary; truncated JSON | 7 |
| Abstract truncation keeps under the token budget and marks the paper truncated | 1 |
| One paper fails, others succeed → digest has both | 7 |
| Digest markdown is stable for a fixed input (golden file) | 1, 9 |
| Eval metrics on a known confusion matrix | 10 |

All HTTP tests use `httptest`; no network in `go test`.
