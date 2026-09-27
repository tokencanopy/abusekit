# abusekit

A small, model-agnostic abuse-scoring service and Go library. Products push typed events about a
subject (an account); abusekit keeps a risk score per subject with the signals behind it; products
read the score and decide what to do. It never enforces.

- Two integration calls: `POST /v1/events`, `GET /v1/subjects/{id}`.
- Rules are YAML; scorers are adapters (Jev, Laya, Gemini, ensembles); modes per rule (shadow, advise).
- Pure `score()` core, JSONL corpus, `abusekit eval` harness, CI gate on precision/recall/calibration.
- One binary (service) or importable package; Postgres only (v1 defers SQLite/embedded mode — see
  the design's §4.13 alternatives).

Design: `docs/design/2026-09-27-abusekit-design.md`. Status: S1 (core types, Postgres
store, the model seam + local scorer, the pure Plan/Combine core, and the rules/vendors config
loader) is implemented; see `docs/plans/2026-09-27-v0-plan.md` (on `main`) for the full slice plan.
The HTTP surface, feature extraction, and worker land in later slices.

## Development

```bash
go build ./...              # cmd/abusekit
go test -short ./...        # fast, no Postgres required
go test ./...                # full suite, including internal/store's DB-backed tests
```

The DB-backed tests need Postgres at `ABUSEKIT_TEST_DATABASE_URL` (defaults to
`postgres://e2a:e2a@localhost:5433/abusekit_test?sslmode=disable`); they self-provision the target
database if it doesn't exist yet and skip cleanly if no server is reachable at all. See the
`Makefile` (`make build`, `test`, `test-db`, `lint`, `gate`) for the same commands wrapped as
targets, and `AGENTS.md` for the worktree/lint/corpus conventions that apply to every change.

```bash
go run ./cmd/abusekit migrate --database-url postgres://e2a:e2a@localhost:5433/abusekit_dev
go run ./cmd/abusekit serve --check --database-url postgres://e2a:e2a@localhost:5433/abusekit_dev
```
