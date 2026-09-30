# Exact scoring baseline

`reference-flat.jsonl` (AMD64) and `reference-flat-arm64.jsonl` (ARM64) are the
P0 oracles for the generic-feature migration. Each is
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

On ARM64, use `--golden-check eval/golden/reference-flat-arm64.jsonl`.
`go test ./eval` selects the reference for the executing CPU and performs the same comparison. A mismatch is a failure, not an
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

Each row names its CPU architecture and stores every feature, combined score, and per-rule risk as 16-digit
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

## Why there are two exact files

A single cross-CPU bit oracle is not valid for the existing scorer. With identical
source, Go 1.26.1, features, coefficients, and iteration order, ARM64 and AMD64
weighted sums differ in their final bits. For example the `fastonb-evt-007` sum
is `bfd4fc9f493bb207` on ARM64 and `bfd4fc9f493bb206` on AMD64. Those differences
propagate through the sigmoid. Across this corpus, 499 risks/scores differ; all
features, input hashes, scorer versions, timers, tiers, and flags are identical.
Linux ARM64 also matches macOS ARM64. This is an existing CPU arithmetic
difference, not an intended scoring change.

Both files retain exact bit comparison with no tolerance and no changes to the
scorer. The architecture field makes accidental cross-CPU comparisons fail even
for inputs whose scores happen to match. P1 must prove parity separately on both
CPUs; a new CPU needs its own deliberately reviewed oracle.
