# pubtrack analysis — 2026-10-19

Eval set: 4 papers (3 labeled for relevance) · model `llama3.2:3b` · judge `llama3.1:8b` · embeddings `nomic-embed-text`

## Summary fact-check

`llama3.1:8b` checked each summary against its abstract. A flag is the judge's opinion, not ground truth: read the abstract before trusting it.

| Checked | Faithful | Flagged | Judge failed | No summary |
|---|---|---|---|---|
| 2 | 1 | 1 | 1 | 1 |

### Flagged (1)

#### [Remote cognitive testing at scale with a very long title that needs to be shortened for the table](https://pubmed.ncbi.nlm.nih.gov/102/)

The study enrolled 500 participants.

- 500 participants is not in the abstract

### Judge failed (1)

- [CAR-T outcomes \[phase 2\]](https://pubmed.ncbi.nlm.nih.gov/103/) — invalid model output: missing "verdict"

## Structured extraction

| Field | Correct | Labeled | Accuracy |
|---|---|---|---|
| Study design | 1 | 2 | 0.50 |
| Sample size | 2 | 2 | 1.00 |
| Setting | 1 | 1 | 1.00 |

### Mismatches (1)

- [Remote cognitive testing at scale with a very long title that needs to be shortened for the table](https://pubmed.ncbi.nlm.nih.gov/102/) — design: labeled `observational`, extracted `validation`

### Extracted fields

| Paper | Design | n | Setting | Technology |
|---|---|---|---|---|
| [In-home gait sensing \| pilot](https://pubmed.ncbi.nlm.nih.gov/101/) | observational | 40 | home | gait mat, motion sensors |
| [Remote cognitive testing at scale with a very long title tha…](https://pubmed.ncbi.nlm.nih.gov/102/) | validation | 120 | online | smartphone app |
| [CAR-T outcomes \[phase 2\]](https://pubmed.ncbi.nlm.nih.gov/103/) | failed: invalid model output: setting "moon" is not one of: home | — | — | — |

## Similarity to your papers

Score = highest cosine similarity between a paper and any of your 6 papers, using `nomic-embed-text`.

Mean score: relevant 0.81 · not relevant 0.66 · ranking AUC 1.00 (0.50 is chance; 1.00 means every relevant paper outscores every other)

| Keep if ≥ | Kept | Relevant kept | Recall | Precision |
|---|---|---|---|---|
| 0.65 | 3 | 2/2 | 1.00 | 0.67 |
| 0.70 | 2 | 2/2 | 1.00 | 1.00 |
| 0.75 | 2 | 2/2 | 1.00 | 1.00 |
| 0.80 | 1 | 1/2 | 0.50 | 1.00 |
| 0.85 | 0 | 0/2 | 0.00 | n/a |

| Paper | Label | Score | Closest |
|---|---|---|---|
| [In-home gait sensing \| pilot](https://pubmed.ncbi.nlm.nih.gov/101/) | y | 0.83 | JMIR Form Res 2020 (veterans in-home sensing) |
| [Remote cognitive testing at scale with a very long title tha…](https://pubmed.ncbi.nlm.nih.gov/102/) | y | 0.78 | Gerontology 2021 (SMART) |
| [Unlabeled paper](https://pubmed.ncbi.nlm.nih.gov/104/) | — | 0.70 | Gerontologist 2025 (reminiscence walking) |
| [CAR-T outcomes \[phase 2\]](https://pubmed.ncbi.nlm.nih.gov/103/) | n | 0.66 | Digit Biomark 2020 (CART) |

## Run summary

| | |
|---|---|
| Model calls | 9 |
| Tokens in / out | 7,200 / 900 |
| Model time | 21.0s |
