# pubtrack (Go): sequence diagrams

Two diagrams: `pubtrack run`, the weekly cron job, and the commands that check a setup or measure
the model (`check`, `eval`, `bench`, `analyze`, `factcheck`). Participant names are the Go packages
that do the work.

## `pubtrack run`

```mermaid
sequenceDiagram
    autonumber
    actor U as cron / user
    participant M as cmd/pubtrack<br/>main.go
    participant CF as config.json<br/>state.json
    participant R as internal/run<br/>Runner
    participant PM as internal/pubmed<br/>Client
    participant N as NCBI E-utilities
    participant L as internal/llm<br/>Client
    participant O as Ollama
    participant D as digests/

    U->>M: pubtrack run [-config] [-dry-run]
    M->>M: signal.NotifyContext(SIGINT, SIGTERM)
    M->>CF: config.Load, state.Load
    M->>M: prompts.Render(prompt_version, profile)<br/>(prompts embedded in the binary)

    Note over M,O: Check Ollama before spending any NCBI requests
    M->>L: Preflight
    L->>O: GET /api/version (needs 0.5 or later)
    L->>O: GET /api/tags (is the model pulled?)
    M->>L: Warmup
    L->>O: POST /api/generate, no prompt, keep_alive 10m<br/>(loads the model, uses the load timeout)
    opt embedding_model is set
        M->>L: Preflight on an embedding client
        L->>O: GET /api/version, GET /api/tags
        Note right of M: On failure: print "similarity off" and go on
    end

    M->>R: Run(ctx)

    Note over R,N: 1. Search
    loop each query in config.queries
        R->>PM: Search(term, window_days)
        PM->>PM: limiter.Wait (3 req/s, 10 with an API key)
        PM->>N: GET esearch.fcgi (datetype=edat, reldate, retmax=500)
        N-->>PM: PMIDs + count (JSON)
        Note right of PM: 429, 5xx and connection errors retried up to 4 times<br/>with exponential backoff or Retry-After
    end
    Note right of R: One failed query is a warning, all failed is an error

    Note over R,N: 2. Citations of the reader's papers
    opt own_papers is not empty
        R->>PM: CitedBy(own PMIDs)
        PM->>N: GET elink.fcgi (linkname=pubmed_pubmed_citedin)
        N-->>PM: citing PMID to cited PMIDs
        Note right of R: First run only: citing papers outside the window<br/>are marked seen ("baseline")
    end

    Note over R: 3. Candidates = (query hits + citing papers + state.Failed)<br/>minus own papers minus already seen, sorted
    alt no candidates
        R-->>M: empty digest
    end

    Note over R,N: 4. Fetch
    R->>PM: Fetch(candidates, plus own papers if similarity is on)
    loop batches of 100
        PM->>N: GET efetch.fcgi (XML)
        N-->>PM: PubmedArticleSet
    end

    Note over R,O: 5. Similarity (optional)
    opt Embedder set
        R->>L: Embed(own papers + candidates), one request
        L->>O: POST /api/embed
        O-->>L: vectors
        R->>R: closest own paper per candidate (cosine)
        Note right of R: Any failure becomes a warning
    end

    Note over R,O: 6. Summarize, one paper at a time
    loop each candidate PMID
        alt not returned by efetch
            R->>R: fail: state.MarkFailed (gives up after max_attempts)
        else erratum
            R->>R: state.MarkSeen, skip
        else no abstract
            R->>R: state.MarkSeen, note "not scored"
        else Ollama stopped responding earlier
            R->>R: state.KeepPending (retry next run, no attempt counted)
        else has an abstract
            R->>R: ownwork Matcher.Mentions(platform names)
            R->>L: Summarize(paper)
            loop up to 2 attempts
                L->>O: POST /api/chat (JSON schema, temperature 0)
                O-->>L: reply
                L->>L: Validate (on a bad reply, add it plus<br/>what was wrong to the chat and retry)
            end
            alt valid
                L-->>R: summary, relevance 1 to 5, reason
                R->>R: state.MarkSeen
                opt extract_fields
                    R->>L: extract.Run(paper)
                    L->>O: POST /api/chat (extraction schema)
                    Note right of R: Failure is noted on the item only
                end
            else ErrUnavailable
                R->>R: state.KeepPending, stop calling Ollama
            else other error
                R->>R: fail: state.MarkFailed
            end
        end
    end
    R-->>M: Digest (state updated in memory only)

    alt -dry-run
        M-->>U: digest markdown on stdout, nothing written
    else normal run
        opt the digest has items
            M->>D: digest-DATE.json, digest-DATE.md<br/>(adds -2, -3 rather than overwrite)
        end
        M->>CF: state.Save (temp file, fsync, rename)
        Note right of M: State is saved after the digest so a failed write<br/>brings the same papers back next run
        M-->>U: exit 0
    end
```

## `check`, `eval`, `bench`, `analyze`, `factcheck`

```mermaid
sequenceDiagram
    autonumber
    actor U as user
    participant M as cmd/pubtrack<br/>main.go, eval.go, analyze.go
    participant E as eval/<br/>pmids.txt, labels.csv,<br/>papers.json
    participant PM as internal/pubmed
    participant N as NCBI
    participant L as internal/llm
    participant O as Ollama
    participant RS as eval/results/<br/>digests/

    alt check
        U->>M: pubtrack check
        M->>L: Preflight
        L->>O: GET /api/version, GET /api/tags
        loop each query
            M->>PM: Search
            PM->>N: GET esearch.fcgi
        end
        M-->>U: config, Ollama and per-query counts (exit 1 on any failure)

    else eval or bench
        U->>M: pubtrack eval [-model] or bench [-models a,b,c]
        opt bench
            M->>L: Preflight every model first
            L->>O: GET /api/tags
            Note right of M: Lists "ollama pull ..." for missing models and stops
        end
        M->>E: ReadPMIDs, ReadLabels, LoadPapers (cache)
        opt papers missing from the cache
            M->>PM: Fetch(missing)
            PM->>N: GET efetch.fcgi
            M->>E: SavePapers
        end
        loop each model (one model for eval)
            M->>L: Preflight, ModelSize
            opt bench
                M->>L: Unload
                L->>O: POST /api/generate keep_alive 0 (cold load)
            end
            M->>L: Warmup (timed)
            loop each eval paper
                M->>L: Summarize
                L->>O: POST /api/chat
            end
            M->>M: precision, recall, etc. against labels
            M->>RS: results/DATE-go-MODEL-PROMPT.json
        end
        opt bench
            M->>RS: results/DATE-go-bench-PROMPT.md
        end
        M-->>U: report on stdout

    else analyze
        U->>M: pubtrack analyze [-judge llama3.1:8b]
        M->>E: read pmids, labels, extraction_labels
        Note right of M: Refuses if any paper is unlabeled (unless -force)
        M->>L: Preflight summarizer, judge, embedding model
        M->>E: papers.json (fetching misses, incl. own papers)
        M->>L: Warmup summarizer
        loop each eval paper
            M->>L: Summarize, then extract.Run
            L->>O: POST /api/chat (x2)
        end
        M->>L: Warmup judge
        Note right of M: Judge runs after the small model so the two<br/>aren't swapped in and out of memory per paper
        loop each summary
            M->>L: factcheck.Check(abstract, summary)
            L->>O: POST /api/chat (judge model)
        end
        M->>L: Embed(own papers + eval papers)
        L->>O: POST /api/embed
        M->>RS: results/DATE-go-analysis-MODEL.md + .json

    else factcheck
        U->>M: pubtrack factcheck -digest digests/digest-DATE.json
        M->>RS: read the digest JSON
        M->>L: Preflight, Warmup judge
        loop each summarized item with an abstract
            M->>L: factcheck.Check
            L->>O: POST /api/chat (judge model)
        end
        M->>RS: digests/digest-DATE-factcheck.md
    end
```
