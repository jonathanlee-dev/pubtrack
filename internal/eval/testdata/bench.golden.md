## pubtrack bench — prompt v1 · format schema · relevant = score ≥ 4

| Model | Size | Load | Avg per paper | Precision | Recall | Agreement | Failed | Repaired |
|---|---|---|---|---|---|---|---|---|
| llama3.2:1b | 1.3 GB | 1.2s | 2.0s | 0.00 (0/1) | 0.00 (0/2) | 0.25 (1/4) | 1 | 1 |
| llama3.2:3b | 2.0 GB | 2.1s | 2.0s | 0.50 (1/2) | 0.50 (1/2) | 0.50 (2/4) | 1 | 0 |

5 labeled papers (3 relevant). Every model saw the same prompt, labels, and abstracts at temperature 0. Load is the time to load the model after unloading it from Ollama.
