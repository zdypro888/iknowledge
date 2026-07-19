# Semantic retrieval regression baseline

`v1/qrels.jsonl` is a deterministic regression fixture for iknowledge's Flat
semantic retrieval algorithm. It verifies four contracts without network or a
model runtime:

- relevant `current`, `risk`, and `history` nodes reach Top-K;
- records stay in their human-labelled evidence lane;
- multiple cards from one node cannot occupy multiple positions in one lane;
- the exact winning record and stable order in every lane match the checked-in
  expectation, including same-node card ties.

Run the strict baseline from the repository root:

```bash
go run ./cmd/kbsemeval --input eval/semantic/v1/qrels.jsonl
```

The default gate is intentionally strict: every non-empty lane must have
`Recall@K = 1`, lane precision and distinct-node rate must both equal `1`, all
three lanes must have qrels, and ranking violations must equal `0`. Exit code
`0` means pass, `1` means bad input or a quality regression, and `2` means
invalid CLI usage.

The safety statement “similarity discovers evidence; it never adjudicates” is
not a vector metric and is deliberately not faked from fixture annotations.
Production behavior is covered by engine-level tests of recall rendering and
the task decision advisory (`semantic_decision_advisory_test.go`), which assert
that risk/history remain advisory and do not block task creation.

This is an **algorithm regression baseline, not a real embedding-model quality
claim**. The checked-in vectors are small, hand-authored fixtures. To evaluate
a real local or remote model, keep human qrels independent and add a new
versioned JSONL file using vectors precomputed by that model. Never tune qrels
from the model output. The evaluator itself remains offline and sends no text
or vectors to a provider.

## JSONL schema v1

Each non-empty line is one case with:

- `schema`: exactly `iknowledge-semantic-qrels-v1`;
- `id`, `top_k`, and one `query_vector`;
- `records`: record ID, node ID, observed `lane`, human `expected_lane`, and
  vector;
- `qrels`: relevant node IDs grouped by `current`, `risk`, and `history`.
- `expected_hits`: exact ordered winning record IDs for all three lanes. Empty
  lanes are represented by an explicit empty array.

All vectors in one case must have the same dimensions. A qrel node must have a
record with the matching human `expected_lane`. Unknown fields, lanes, schema
versions, malformed vectors, duplicate IDs, and duplicate qrels fail closed.

For exploratory real-model runs, thresholds can be lowered explicitly with
`--min-recall`, `--min-lane-precision`, and
`--min-distinct-node-rate`. The committed fixture and CI-facing test always use
the strict defaults; do not lower them to hide a regression.
