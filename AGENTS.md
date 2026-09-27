# AGENTS.md

## What this repo is
abusekit: model-agnostic abuse scoring for TokenCanopy products (first consumer: e2a). Score-only —
it never pauses, holds or blocks; callers own policy. See `docs/design/` for the authoritative
design; implementation follows it slice by slice (design → implement → review).

## Conventions
- Go, single module, single binary `cmd/abusekit`; library packages under `internal/` except the
  public `pkg/abusekit` client surface.
- Vendor SDKs may be imported only inside `internal/model/<adapter>/`. A lint test enforces it.
- Every adapter passes `internal/model/contract_test.go`.
- `make gate` runs the committed corpus through configured scorers; CI fails below the floors in
  `eval/floors.yaml`.
- Never commit real customer or operator data; the corpus is redacted synthetic-or-anonymised.
- Work in git worktrees under `.worktrees/` (gitignored); never in the root checkout.
