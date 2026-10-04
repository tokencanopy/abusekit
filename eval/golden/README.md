# Exact scoring baseline

`reference-ns.jsonl` (AMD64 with FMA), `reference-ns-amd64-no-fma.jsonl`,
and `reference-ns-arm64.jsonl` are the
P1 oracles for the generic-feature migration. Each is
built exclusively from the committed synthetic event fixtures: all root-level
`eval/fixtures/*.jsonl` and `eval/fixtures/synthetic/events.jsonl`. Each file is an
isolated replay world. No labels-file answer key, private dataset, database, or
vendor model is read.

Run from the repository root:

```sh
go run ./cmd/abusekit eval --golden \
  --brands-extra eval/fixtures/test_brands.yaml \
  --golden-check eval/golden/reference-ns.jsonl
```

On ARM64, use `--golden-check eval/golden/reference-ns-arm64.jsonl`.
On AMD64 without FMA, use `--golden-check eval/golden/reference-ns-amd64-no-fma.jsonl`.
Set `GODEBUG=cpu.fma=off` before starting the process to exercise that math path.
`go test ./eval` selects the reference from the executing CPU capabilities and performs the same comparison. A mismatch is a failure, not an
instruction to regenerate. To propose an intentionally reviewed new oracle,
replace `--golden-check` with `--out /tmp/candidate-golden.jsonl` and inspect the
diff. Keep both the flat and namespaced oracles through later slices.

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

Each row names its numeric profile and stores every feature, combined score, and per-rule risk as 16-digit
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

## Why there are three exact files

A single cross-CPU bit oracle is not valid for the existing scorer. ARM64 and
AMD64 weighted sums differ in their final bits. For example the
`fastonb-evt-007` sum is `bfd4fc9f493bb207` on ARM64 and
`bfd4fc9f493bb206` on AMD64. Across this corpus, 499 risks/scores differ
between ARM64 and AMD64 without FMA.

AMD64's Go `math.Exp` also selects an FMA implementation when AVX and FMA are
available. Native Linux CI differs from the no-FMA AMD64 oracle at 94 points;
disabling FMA in that same CI runner gives exact equality. Go 1.23.0 and 1.26.1
produce identical no-FMA outputs locally. These are existing arithmetic paths,
not intended scoring changes. All profiles have identical point identities,
features, input hashes, scorer versions, timers, tiers, and flags.

`GoldenProfile` selects the file independently of scores using runtime CPU
capabilities (`HasAVX && HasFMA`, matching Go's math dispatch). It never tries
references until one matches. Baselines use Go 1.23.0 and `GOAMD64=v1`; native
ARM64 and AMD64 without FMA were also verified on Go 1.26.1. Other toolchains or
AMD64 build levels require deliberate verification; higher build levels may
make FMA mandatory and ignore runtime disabling. CI exercises native FMA,
forced no-FMA, and ARM64. Profile metadata makes cross-profile checks fail.

Every file retains exact bit comparison with no tolerance and no scorer changes.
P1 must prove parity on all three profiles. Unknown profiles have no reference
and fail instead of skipping. Keep the flat oracles through the migration.

## P0-to-P1 migration proof

The original `reference-flat*.jsonl` files remain byte-for-byte unchanged.
`TestGoldenRenameParity` checks all three against `reference-ns*.jsonl`, using
an explicit test-only rename map. Only feature keys, input hashes, and local
scorer versions may change. Every feature value, risk, score, timer, tier and
flag must remain bit-exact. Native replay then separately checks the renamed
oracle; hashes and versions cannot drift after P1.

The namespaced files were constructed by preserving each P0 profile's numeric
values, renaming keys, and recording the new hashes/versions from the ARM64
replay (those identities depend only on the identical feature maps). CI runs
native replay on all three profiles to verify those files independently.
