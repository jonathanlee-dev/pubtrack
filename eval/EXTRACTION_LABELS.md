# Extraction labels (optional)

`extraction_labels.csv` scores the structured-extraction section of `pubtrack analyze`. Label as
many papers as you like; 8 to 10 is enough to see where the model goes wrong. Leave a cell blank to
skip that field for that paper. Use the abstracts in `LABELING.md`.

| Column | Allowed values |
|---|---|
| `study_design` | randomized controlled trial, observational, feasibility or pilot, validation, qualitative, review, protocol, other, not stated |
| `sample_size` | a whole number of participants, or `0` if the abstract doesn't say |
| `setting` | home, community, clinic or hospital, long-term care, laboratory, online, other, not stated |

"validation" means a study that develops or validates a measure or tool. "review" includes
systematic reviews and meta-analyses. Population and technology are free text, so they're shown
in the report but not scored.
