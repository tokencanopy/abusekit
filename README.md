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
(feature extraction, same-tenant linking, the worker), S3 (the HTTP surface + Go client), and S4
(the evaluation harness + CI gate, below) are implemented; see `docs/plans/2026-09-27-v0-plan.md`
for the full slice plan. Vendor adapters (S5) and the e2a integration (S6+) land in later slices.

## HTTP surface

`cmd/abusekit serve` exposes design §4.3/§4.9's API on `--listen` (default `127.0.0.1:8080`; empty
disables it). Every request is signed (design §4.3, amended §4.2 for the nonce):
`X-Abusekit-Key: <key id>`, `X-Abusekit-Timestamp: <RFC3339>`, `X-Abusekit-Nonce: <32..128 hex chars,
16..64 random bytes>`, `X-Abusekit-Signature: hex(hmac-sha256(secret,
method\npath?query\ntimestamp\nkey_id\nnonce\nsha256(body)))`. The nonce must be fresh per request —
a client retrying a failed call signs each attempt with its own nonce (`pkg/abusekit` does this
automatically). Timestamps outside ±5 min are rejected; a `backfill`-scoped key gets a wider ±24h
window instead, but ONLY on `POST /v1/events` — every other endpoint uses the ordinary ±5min window
even for that key, since there's no legitimate reason for a backdated timestamp anywhere but a
historical event import. A replayed `(key, nonce)` pair is rejected for as long as its timestamp
could still independently pass whichever window applied (±5min or the backfill ±24h) — the two share
one formula so the replay window can never end before the accepted-timestamp window does. A request
that authenticates but is denied for lacking scope never consumes its nonce (a caller can retry the
identical nonce once properly scoped). Keys and their scopes (`events`/`labels`/`read`/`backfill`)
are loaded from `--keys`/`ABUSEKIT_KEYS_CONFIG` — **required, no default** — see `config/keys.yaml`
for the shape; every secret it ships is prefixed `dev-only-` and refused at boot unless `--dev` is
passed, and every secret must be ≥32 bytes regardless. Production points `--keys` at a file generated
from a real secret store, run without `--dev`, never a committed one.

A coarse per-IP limit guards against a flood of FAILED authentications (bad signature, unknown key,
stale timestamp, malformed nonce) from one IP — it is consulted only once a request has already
failed to authenticate, and a request that authenticates successfully never counts against it no
matter how many prior failures that IP racked up. It does not bound the cost of verifying a
signature itself (every request's HMAC is still computed and checked exactly once, pass or fail) —
that computation is cheap enough (microseconds) not to be the thing worth rate-limiting; what this
guards against is a shared IP (a NAT gateway, a corporate proxy) getting treated as abusive and
having its LEGITIMATE traffic blocked, which an earlier "block before checking the signature at all"
design did. A tighter per-key limit applies to `/v1/events` after a request authenticates (never on
the raw, unverified key header).

**Replay-cache memory bound:** the server remembers `(key, nonce)` pairs (as a 16-byte hash, not the
raw nonce string) for as long as a replay of them could still pass the timestamp window above — ~5min
for an ordinary key, ~24h for a `backfill`-scoped key on `/v1/events`. Each entry costs roughly 63
bytes (the 16-byte key, a `time.Time` value, and Go map bucket overhead); only VALID, fully
authenticated AND properly scoped requests ever reach this cache (an invalid signature or an unknown
key is rejected before the check, and consumes nothing here — see the per-IP limit above for that
traffic instead). A single backfill-scoped key sending a sustained 50 req/s for the full 24h window
accumulates on the order of 50 × 86400 ≈ 4.3M entries, ≈ 270MB — a real number to budget for if a
producer's backfill throughput is expected to run anywhere near that, not an unbounded growth risk
under normal (or even a hostile-but-unauthenticated) load.

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

## Evaluation harness (`eval`, design §4.10)

`eval` is a pure, importable Go package (`github.com/tokencanopy/abusekit/eval`): it never opens a
network connection or reads a product database (the one exception, `abusekit corpus export`, lives
in `cmd/abusekit`, not in `eval` itself). `eval.Run(ctx, Dataset, Rule, Scorer, Options) (RunResult,
error)` scores every subject in a `Dataset` and reduces the result to `RunResult{Manifest, Verdicts,
Metrics}` — precision/recall/F1 at the rule's threshold AND at each global tier cut (each with a
Wilson 95% interval), a confusion matrix, 10-bin ECE, AUROC, latency p50/p95, cost, and — for a
replay-shaped dataset — a lead-time histogram (the earliest of `first_send`/`early_15m`/`full` each
positive subject was first flagged at).

Two corpus shapes (design §4.6), both JSONL:

- **Label-snapshot**: `{id, input:{features,text,context}, label, split, source, meta}` — one row
  per already-resolved decision point. Schema: `eval/schema/corpus-v1.schema.json`.
- **Event-replay pair**: an events file (`{subject,type,at,data,links?}`, internal/event's own wire
  shape) plus a labels file (`{subject,label,category?,source,decision_at:{<slice>:<RFC3339>}}`).
  Each subject's feature vector is rebuilt from events STRICTLY BEFORE its decision_at, using
  internal/feature with same-tenant neighbour evidence resolved from an in-memory index built from
  the dataset's own links — evaluated as of each subject's own decision point, never as of the end
  of the whole dataset (see `eval/neighbors.go`'s `evidenceAsOf`).

```bash
go build -o abusekit ./cmd/abusekit

# Score the committed synthetic corpus against the shipped local weights.
./abusekit eval --dataset eval/fixtures/synthetic/events.jsonl \
  --labels eval/fixtures/synthetic/labels.jsonl \
  --rule new_account_velocity --scorer local --slice full --out run.json

# Compare every registered scorer side by side (a table on stdout).
./abusekit eval --dataset ... --labels ... --rule new_account_velocity --scorer all

# CI's gate: exit 0 pass, 1 a floor was violated, 2 bad input.
./abusekit eval --dataset ... --labels ... --rule new_account_velocity --scorer local \
  --slice full --floors eval/floors.yaml

# A raw scoring pipe for an external framework — stdin/stdout JSONL, no persistence.
echo '{"id":"x","input":{"features":{"subject_age_h":0.1}}}' | \
  ./abusekit score --jsonl --rule new_account_velocity --scorer local

# Export labelled corpus_examples rows (design §4.9) as a corpus-v1 file
# `abusekit eval` can read straight back in. KNOWN GAP: nothing yet marks
# a row `gated` (design's "a label enters the gate corpus only after a
# second source agrees") — every row is exported regardless, with its
# `gated` column surfaced in `meta.gated` (see corpus_cmd.go).
./abusekit corpus export --database-url "$ABUSEKIT_DATABASE_URL" --tenant e2a --split all > corpus.jsonl
```

**The private incident corpus** (the real September 2026 accounts — see design §1/§4.10, plan.md's
S9) lives in a separate private repository and is never committed here (AGENTS.md: "Never commit
real customer or operator data"). Run it with the exact same binary and no code changes, pointing
`--dataset`/`--labels` at that repo's checkout:

```bash
./abusekit eval --dataset <private-repo>/events.jsonl --labels <private-repo>/labels.jsonl \
  --rule new_account_velocity --scorer local --slice full --out incident-run.json
```

**Cassettes** (design §4.10) record/replay a vendor scorer's answers, keyed by `(scorer,
scorer_version, model, prompt_version, input_hash)` — a weights/checkpoint change invalidates the
cache entry the same way a rule's own input hash does. `--cassettes <dir>` points at a directory of
per-scorer cassette files; add `--record` to call the live scorer and write new entries, omit it to
replay only. Every scorer OTHER than `local` requires one or the other — running a non-`local`
scorer with neither flag is a bad-input error (exit 2), never a silent live call, and CI always
passes `--cassettes` with no `--record`, so a genuine miss is a loud, wrapped error
(`eval.ErrCassetteMiss`). A cassette also records which corpus (`dataset_sha`) it was built
against; a mismatch at load time is the same loud failure. The `local` scorer never needs one (no
network to record in the first place); v0 has no other scorer registered yet (S5 adds
`jev`/`laya`/`gemini`).

**The synthetic corpus** (`eval/fixtures/synthetic/`, generated by the seeded `eval/gen` command —
see `eval/fixtures/README.md`) is what `make gate`/CI score against; `eval/floors.yaml` documents how
its floors were set and how to raise them.

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
