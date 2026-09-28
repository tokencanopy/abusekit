# abusekit

A small, model-agnostic abuse-scoring service and Go library. Products push typed events about a
subject (an account); abusekit keeps a risk score per subject with the signals behind it; products
read the score and decide what to do. It never enforces.

- Two integration calls: `POST /v1/events`, `GET /v1/subjects/{id}`.
- Rules are YAML; scorers are adapters (Jev, Laya, Gemini, ensembles); modes per rule (shadow, advise).
- Pure `score()` core, JSONL corpus, `abusekit eval` harness, CI gate on precision/recall/calibration.
- One binary (service) or importable package; Postgres only (v1 defers SQLite/embedded mode — see
  the design's §4.13 alternatives).

Design: `docs/design/2026-09-27-abusekit-design.md`. Status: S1 (core types, Postgres store, the
model seam + local scorer, the pure Plan/Combine core, and the rules/vendors config loader), S2
(feature extraction, same-tenant linking, the worker), and S3 (the HTTP surface + Go client, below)
are implemented; see `docs/plans/2026-09-27-v0-plan.md` for the full slice plan. The evaluation
harness/CI gate (S4), vendor adapters (S5), and the e2a integration (S6+) land in later slices.

## HTTP surface

`cmd/abusekit serve` exposes design §4.3/§4.9's API on `--listen` (default `127.0.0.1:8080`; empty
disables it). Every request is signed (design §4.3, amended §4.2 for the nonce):
`X-Abusekit-Key: <key id>`, `X-Abusekit-Timestamp: <RFC3339>`, `X-Abusekit-Nonce: <>=16 random bytes,
hex>`, `X-Abusekit-Signature: hex(hmac-sha256(secret,
method\npath?query\ntimestamp\nkey_id\nnonce\nsha256(body)))`. The nonce must be fresh per request —
a client retrying a failed call signs each attempt with its own nonce (`pkg/abusekit` does this
automatically). Timestamps outside ±5 min are rejected (unless the key has the `backfill` scope); a
replayed `(key, nonce)` pair is rejected for as long as its timestamp could still independently pass
the skew check (a backfill-scoped key, which skips that check, is protected for 24h instead). Keys
and their scopes (`events`/`labels`/`read`/`backfill`) are loaded from `--keys`/`ABUSEKIT_KEYS_CONFIG`
— **required, no default** — see `config/keys.yaml` for the shape; every secret it ships is prefixed
`dev-only-` and refused at boot unless `--dev` is passed, and every secret must be ≥32 bytes
regardless. Production points `--keys` at a file generated from a real secret store, run without
`--dev`, never a committed one.

A coarse per-IP rate limit applies before authentication on every route; a tighter per-key limit
applies to `/v1/events` after a request authenticates (never on the raw, unverified key header).

| Method | Path | Scope | Notes |
| --- | --- | --- | --- |
| `POST` | `/v1/events` | `events` | Batch of 1–100, `{"events":[...]}`, `application/json`, ≤1 MiB. `202` with `{accepted, duplicates, rejected:[{index,code,message}]}` — the whole request only 400/401/403/413/415/429s; a bad individual event is a per-item rejection. |
| `GET` | `/v1/subjects/{subject}` | `read` | Design §4.4's score shape. `404` only for a subject never seen by the caller's own tenant (a different tenant's subject also 404s — no cross-tenant existence disclosure). `ETag` (hashes the verdict ids AND the subject's dirty/scored sequence counters, so it changes the instant the subject is dirtied again, not just when a new verdict lands) on the response; `If-None-Match` (strong or weak `W/"..."`) → `304`. `Cache-Control: no-store`. |
| `POST` | `/v1/subjects/{subject}/evaluate` | `read` | `{"deadline_ms": 1..3000}` (default 3000). Scores synchronously via the SAME lease/timeout/budget path the worker's own tick uses, bounding only the scorer-call phase (never the DB reads); a rule not scored by `local` carries its last scored result forward rather than being overwritten, until a vendor scorer's measured p99 justifies running it inline. A `internal`/`synthetic` subject, or one cut short by the deadline, still returns `200` with the current stored view and `evaluated_now:false` (plus `evaluate_note`), never a `500`. Rate-limited to 1/s per subject (`429` + `Retry-After`); a subject claimed elsewhere is `409 subject_busy` with a real `Retry-After`. |
| `POST` | `/v1/labels` | `labels` | Design §4.9. `rule` optional (subject-level label is `benign`/`abusive`; a rule-scoped label's vocabulary is that rule's own `labels`). Snapshots a `corpus_examples` row (redacted event slice up to `decision_at`, re-extracted features, an 80/20 split keyed by an HMAC so it can't be predicted from the subject id). |
| `GET` | `/healthz` | — | Unauthenticated liveness. |

Every non-2xx response is `{"error":{"code","message","details?"}}`, including from an unexpected
handler panic (recovered into a clean `500`, never a raw stack trace); `X-Request-Id` is
accepted-or-generated and always echoed back; `X-Content-Type-Options: nosniff` is set on every
response.

**Scope note:** `docs/plans/2026-09-27-v0-plan.md` lists a list endpoint (`GET /v1/subjects`) and an
erasure endpoint (`DELETE /v1/subjects/{subject}`) as deferred to v1. An earlier revision of this PR
included both; a fix round found the erasure semantics unsafe to ship (see the PR's own "Fix round"
section) and pulled both back out, preserving that implementation on `feat/s3b-list-erasure` for a
proper S3b design pass.

### Go client (`pkg/abusekit`)

```go
c := abusekit.New(baseURL, keyID, secret)
receipt, err := c.SendEvents(ctx, []abusekit.Event{{ID: "...", Subject: "...", Type: "subject.created", At: time.Now()}})
subject, err := c.Subject(ctx, "acct_123")
subject, err = c.Evaluate(ctx, "acct_123", 3*time.Second)
label, err := c.Label(ctx, abusekit.LabelRequest{Subject: "acct_123", Label: "abusive", Source: "operator", Actor: "ops@example.test"})
```

No dependency beyond the standard library. Signs every request per the scheme above with a fresh
nonce per attempt; decodes a non-2xx response into a typed `*abusekit.APIError` (`StatusCode`,
`Code`, `Message`, `Details`, `RetryAfter` capped at 60s). Retries (transport error or 429/5xx,
honouring `Retry-After`) apply only to the idempotent calls — `SendEvents`, `Subject` — never to
`Label` (each call creates a new row) or `Evaluate` (a retry could spend the subject's 1/s
rate-limit budget on a call that already succeeded). Response bodies are capped at 4 MiB.

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
