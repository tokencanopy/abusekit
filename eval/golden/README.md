# Exact scoring baseline

`reference-flat.jsonl` is the P0 oracle for the generic-feature migration. It is
built exclusively from the committed synthetic event fixtures: all root-level
`eval/fixtures/*.jsonl` and `eval/fixtures/synthetic/events.jsonl`. Each file is an
isolated replay world. No labels-file answer key, private dataset, database, or
vendor model is read.

Run from the repository root:

```sh
go run ./cmd/abusekit eval --golden \
  --brands-extra eval/fixtures/test_brands.yaml \
  --golden-check eval/golden/reference-flat.jsonl
```

`go test ./eval` performs the same comparison. A mismatch is a failure, not an
instruction to regenerate. To propose an intentionally reviewed new oracle,
replace `--golden-check` with `--out /tmp/candidate-golden.jsonl` and inspect the
diff. Keep the flat oracle through P1; the renamed oracle is a separate file.

The baseline contains 8,671 points across 28 event fixtures plus the synthetic
corpus. Every event (including historical label events) produces a fresh score
for its subject. Events are ordered by `(at, producer, id)`; absent producer means
the empty offline namespace, and absent id gets the existing `replay-line-N`
fallback. Explicit IDs are necessary for invariance under input-file shuffling.
Duplicate `(producer, id)` pairs fail instead of silently replaying twice.

Before each event, all timers through that instant run, ordered by time then
subject. A new score replaces that subject's pending timer. After the final
event, timers drain until none remains. This pins the existing scheduler,
including its five-minute coalescing and its decision not to schedule continuous
age decay after the window timers expire. It does not invent extra timer ticks.

Neighbor evidence includes only events already accepted by the replay, including
within timestamp ties. The subject is scored at the event/timer's exact time;
the neighbor lookup's exclusive cutoff is advanced one nanosecond to include the
accepted current event. Ground-truth labels are never used as evidence.

Each row stores every feature, combined score, and per-rule risk as 16-digit
`math.Float64bits` hex, plus tier, degraded state, per-rule input hash and scorer
version, and exact UTC `NextRescoreAt` (empty when unscheduled). Rules run through
`core.Plan` and `core.Combine` with fresh rule states: the oracle pins calculated
scores, rather than reproducing worker hash-cache reuse. The shipped P0 rule has
no stage condition. Vendor scorers are rejected before replay starts.

The CLI accepts either an event file or a fixture directory as `--dataset`.
`--golden-check` exits 1 on drift, 2 on invalid inputs, and 0 on equality; it does
not rewrite the reference. Normal evaluation flags such as `--skip-invalid`,
`--labels`, and `--scorer` are rejected in golden mode. The one-bit weight mutation
test verifies that even an IEEE-754 coefficient change is detected (including
when sigmoid rounding preserves a score, because the scorer version changes).

Capped neighbor discovery visits kinds and hashes in sorted order, matching the
store before applying the total cap. Every event line must contain exactly one
JSON value. Output files must be outside a fixture input directory and cannot
alias any input, configuration, or reference file (including symlinks and hard
links). Successful output replacement is atomic.
