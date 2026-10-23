# Fact-check — digest-2026-10-19

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

## Run summary

| | |
|---|---|
| Model calls | 9 |
| Tokens in / out | 7,200 / 900 |
| Model time | 21.0s |
