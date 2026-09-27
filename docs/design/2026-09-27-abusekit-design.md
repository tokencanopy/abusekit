# abusekit design — v1

Status: draft for review · 2026-09-27 · owner: Josh Zhang

## 1. Problem statement

In September 2026 a single operator ran two phishing campaigns through e2a. The first churned
through roughly twenty free-tier throwaway accounts (create, send up to the daily cap, delete,
re-register). The second bought a paid plan after several stolen-card declines and staged a lure
campaign from a batch of fake-persona agents, rehearsing against the operator's own inboxes before a
blast. Every signal that would have identified both — key and agent creation velocity,
payment-attempt outcomes, brand-like display names, first-day recipient fan-out, the template text
itself — existed in the product's or the payment processor's data. Nothing looked at them together,
and the only "detector" was a human reading the database. The product's shipped fixes
(external-sending gate for new accounts, soft-delete tombstones, a lower free cap,
payment-before-capacity) close the two holes this operator used; they do not add judgement.

TokenCanopy runs more than one product that mints accounts, takes card payments and lets users
create resources. Each will face the same abuser. Building account-level abuse judgement into each
product separately means several copies of the same prompt, several privacy boundaries and no
shared evidence.

**abusekit** is a small, model-agnostic scoring service and library: products push typed events
about a subject (an account), abusekit keeps a risk score per subject with the signals behind it,
and products ask for the score and decide what to do. It never enforces.

### Success criteria (v1, measurable)

1. A product integrates in under a day with two calls: emit an event, read a score. Measured by
   e2a's integration diff being under 300 lines including tests.
2. Replaying the reference scenario (`eval/fixtures/reference_operator.jsonl`, a synthetic event
   sequence modelled on the incident: signup, several fraud-declined card attempts, a prepaid
   success, a plan upgrade minutes later, a burst of keys and persona agents, then first sends an
   hour after setup) yields a `high` tier before the first send, in shadow, with no rule tuned to
   that sequence specifically.
3. The redacted lure families from the incident score ≥0.9 phishing-or-impersonation on every
   registered scorer against a benign transactional corpus with ≤2% false positive rate at the
   same threshold. Reported by the harness, gated in CI.
4. Switching the scorer for a rule from Jev to Laya or Gemini is a config change; every stored
   verdict records which model produced it.
5. abusekit being down or slow never blocks a product request: emit is fire-and-forget, score reads
   have a served-from-cache fallback, and the product's own policy runs without it.

## 2. Goals and non-goals

**Goals**
- Score-only: `{score, tier, signals, model}` per subject; the caller owns enforcement.
- Push-based ingestion of typed events; no reading of any product's database.
- Model-agnostic: `Scorer` and `Explainer` interfaces; Jev, Laya and Gemini adapters in v1;
  selectable per rule; an ensemble is itself a scorer.
- Rules as data (YAML): which events feed which features, thresholds, scorer, mode.
- Modes per rule: `shadow` (compute, store, never surface in tier), `advise` (surface in tier).
- Pure `score()` core so any evaluation framework can drive it; JSONL corpus; run manifests;
  a harness CLI; a CI gate on precision/recall/calibration.
- Labels: operator decisions flow back as labelled examples.
- Runs as one binary (service) or as a Go package (embedded), Postgres or SQLite.
- Privacy: content minimised and redacted before any model call; verdicts store no content;
  per-adapter data-handling policy declared in config and enforced.

**Non-goals (v1)**
- Enforcement of any kind (pausing, holding, blocking). Not even optional.
- Synchronous per-message scoring on a product's send path. Message-level scanning stays in the
  product (e2a: piguard). abusekit consumes those verdicts as events.
- A dashboard. Score endpoint + CLI; the operator UI lives in each product.
- Cross-product identity resolution. One subject namespace per product.
- Fine-tuning pipelines. The harness reports; training Laya is a separate job that consumes the
  same corpus.
- Fraud-payment decisions. Stripe Radar decides payments; abusekit consumes outcomes.

## 3. Context, constraints, assumptions

**Existing patterns reused**
- e2a's billing sidecar: private Go service in its own container beside the OSS server, HMAC-SHA256
  over the request body with a shared secret, one Postgres, config via env. abusekit deploys the
  same way in ops `docker-compose.prod.yaml` and is proxied by Caddy only for internal paths.
- e2a's `piguard` engine: `Detector` interface with `Inspect(ctx, Request) (*Result, error)`,
  `Result{Flagged, Score, Categories, Status, Provider}`, a Gemini adapter with structured JSON
  output, timeouts, truncation reporting. abusekit's `Scorer` follows the same discipline
  (timeouts fail closed to "unscored", never truncate silently) but returns per-label probabilities
  rather than a single flag.
- e2a's privacy policy commits screening to the paid Gemini tier. abusekit's Gemini adapter must
  use the same key class; other adapters must declare their data-handling terms in config
  (`retains_inputs: false`, `trains_on_inputs: false`) or be limited to redacted feature bags.

**Assumptions (unconfirmed, tracked in §8)**
- A1. TypeSafe's Jev early-access terms permit sending redacted account features and subject lines;
  retention and training-on-input terms are acceptable or can be contracted.
- A2. Laya's published checkpoint runs acceptably on CPU for asynchronous jobs (article: ~0.8 s
  untuned CPU); GPU is not required for v1 volume (<10k subjects/day).
- A3. e2a's OSS server can emit events via an internal HTTP hook without a release-coupled schema
  (the event vocabulary is open; unknown types are stored).
- A4. Products can tolerate eventual scores: the ticker recomputes within 60 s of the last event.

## 4. Proposed design

### 4.1 Shape

```
product ──emit(event)──▶  abusekit  ──GET score──▶ product policy
                            │
                            ├─ store (Postgres|SQLite): events, features, verdicts, labels
                            ├─ ticker: for dirty subjects → features → rules → score() → verdict
                            └─ adapters: Scorer{jev, laya, gemini, vote}, Explainer{gemini}
```

Modules and their interfaces (deep where it matters):

| Module | Interface | Notes |
| --- | --- | --- |
| `event` | `Event{Subject, Type, At, Data, Producer}`; `Store.Append`, `Store.Since` | Append-only. Unknown `Type` accepted. Idempotent on `(producer, id)`. |
| `feature` | `Extract(events []Event, window) Features` | Pure. Named numeric/categorical features: velocities, fan-out, payment outcomes, name patterns, scan verdicts. Products get defaults; rules can add derived features. |
| `rule` | `Rule{Name, Mode, Scorer, Explainer?, Inputs, Labels, Threshold}`; loaded from YAML | Data, hot-reloadable. |
| `model` | `Scorer.Score(ctx, ScoreRequest) (ScoreResult, error)`; `Explainer.Explain(ctx, ExplainRequest) (string, error)` | The only seam that touches vendors. Registry by name. |
| `score` | `Score(features, text, rules, scorers) Verdict` | **Pure.** No I/O beyond adapter calls passed in. This is what the harness drives. |
| `serve` | HTTP handlers, HMAC auth, ticker | Thin. |
| `eval` | `Run(dataset, rule, scorer) Manifest+Verdicts`; metrics | CLI `abusekit eval`. |

The deletion test: remove `feature` and every product re-derives velocity and fan-out from raw
events; remove `model` and every rule re-implements vendor quirks and calibration. Both earn their
keep. `serve` is deliberately shallow.

### 4.2 Event API

`POST /v1/events` (batch, ≤100 per call). Auth: `X-Abusekit-Key: <product key>` +
`X-Abusekit-Signature: hex(hmac-sha256(secret, body))`. One key per product; the key names the
producer and scopes subjects.

```json
[{"id":"evt_01J...","subject":"acct_example_1","type":"resource.created","at":"2026-10-01T12:00:00Z",
  "data":{"kind":"agent","name":"Alex Example","address_domain":"agents.example.test"}}]
```

Rules for the body: `id` optional (server mints one; supplying it makes the call idempotent);
`subject` opaque string ≤256; `type` `[a-z_.]+` ≤64; `at` RFC3339 UTC; `data` object ≤8 KiB after
redaction. Responses: `202` `{accepted, duplicates, rejected:[{index, code}]}`; a partial batch is
accepted (per-item codes), never all-or-nothing, so a producer's retry loop stays simple. `400` for
an unparseable body, `401`/`403` for auth, `413` over size, `429` with `Retry-After`.

Built-in vocabulary (all optional; unknown types are stored and available to custom features):

| Type | data (subset) | Feeds |
| --- | --- | --- |
| `subject.created` | `channel`, `email_domain`, `identity_kind` | age, email-domain class |
| `payment.attempt` | `outcome` (`succeeded`\|`declined`\|`blocked`), `reason`, `funding` (`prepaid`\|`debit`\|`credit`), `fingerprint_hash` | declined streak, prepaid-first, fingerprint reuse |
| `subscription.changed` | `plan`, `status`, `amount_minor` | upgrade-within-N-minutes |
| `resource.created` | `kind`, `name`, `address_domain` | creation velocity, name patterns (brand list, `@` in name) |
| `content.sent` | `subject_line`, `recipient_domain`, `recipient_count`, `first_link_host?` | fan-out, distinct domains, subject clusters |
| `content.verdict` | `source`, `category`, `score` | product-side scan results |
| `label` | see §4.6 | operator ground truth |

Redaction happens at ingest, per type: recipient addresses are never accepted (domain only);
`name` and `subject_line` are kept (they are the signal) but capped at 200 chars; free-text
`data` keys not in the vocabulary are hashed unless a rule declares them `text`.

### 4.3 Score API

`GET /v1/subjects/{subject}` → `200`

```json
{"subject":"acct_example_1","score":0.93,"tier":"high",
 "signals":[{"rule":"new_account_velocity","score":0.97,"mode":"advise","model":"jev@2026-09-26",
             "reason":"6 resources in 74s, 4 keys in 45s, upgrade 16m after signup"},
            {"rule":"lure_similarity","score":0.88,"mode":"shadow","model":"laya@ck-3"}],
 "scored_at":"2026-10-01T12:01:30Z","stale":false}
```

`tier` is derived from the max `advise` signal by configured cut points (`low <0.4 ≤ medium <0.8 ≤ high`);
`shadow` signals are returned but excluded from `tier`. `stale:true` when the ticker has not
recomputed since the last event (caller may still act on the last score). `404` only for a subject
never seen; `Cache-Control: no-store`; `ETag` on the verdict id. Never `500` for a scorer failure:
a failed rule is reported as `{"rule":..., "status":"unscored", "error_code":...}` and excluded
from `tier`.

`GET /v1/subjects?tier=high&since=…&cursor=…&limit=100` — paginated, stable sort by `scored_at desc`.

Optional webhook (`subject.tier_changed`) with the same HMAC scheme; at-least-once; consumers
dedupe on `verdict_id`.

### 4.4 Rules (YAML)

```yaml
rules:
  - name: new_account_velocity
    mode: advise
    scorer: jev
    explainer: gemini
    window: 72h
    inputs: [subject_age, resource_velocity_1h, key_velocity_1h, upgrade_delay, declined_streak,
             prepaid_first, name_brand_match, name_has_at, first_day_distinct_domains]
    labels: [benign, suspicious, abusive]
    threshold: {suspicious: 0.6, abusive: 0.8}
  - name: lure_similarity
    mode: shadow
    scorer: laya
    text: [subject_line, first_link_host]
    labels: [benign, phishing, brand_impersonation, scam]
    threshold: {phishing: 0.85, brand_impersonation: 0.85, scam: 0.85}
  - name: consensus
    mode: shadow
    scorer: vote(jev, laya)
    ...
tiers: {medium: 0.4, high: 0.8}
```

Validation at load: unknown scorer, unknown feature, labels >255 (Jev limit), text inputs for an
adapter whose policy forbids text → the rule is rejected and the previous config stays live.

### 4.5 Model layer

```go
type ScoreRequest struct {
    Labels   []string          // closed set for this call
    Features map[string]any    // numeric/categorical, already redacted
    Text     []string          // optional; empty if the adapter policy forbids text
    Context  string            // ≤2k tokens of program state, e.g. rule intent
}
type ScoreResult struct {
    Probs      map[string]float64 // sums to ~1 over Labels
    Model      string             // "jev@2026-09-26", "laya@ck-3", "gemini-2.5-flash@paid"
    LatencyMS  int
    CostMicro  int64              // micro-USD, 0 for local
    Truncated  bool               // input exceeded adapter window; fail closed upstream
}
type Scorer interface{ Name() string; Policy() DataPolicy; Score(context.Context, ScoreRequest) (ScoreResult, error) }
type Explainer interface{ Name() string; Explain(context.Context, ScoreRequest, ScoreResult) (string, error) }
```

Adapters in v1:
- **jev** — hosted, typed decisions with calibrated probabilities; passes `Labels` as choices,
  `Features`+`Text`+`Context` as unstructured state. Enforces 255 labels / 64k tokens; sets
  `Truncated` rather than cutting.
- **laya** — local weights (Apache-2.0), encoder; 512-token window; temperature-fitted on the
  corpus (`abusekit calibrate laya`), fit stored with the checkpoint id. CPU by default.
- **gemini** — structured-output JSON; `Scorer` and `Explainer`; paid-tier key required, the
  adapter refuses to start on a free-tier key (mirrors e2a's policy).
- **vote(a,b,…)** — mean of member probabilities; records members in `Model`.

Every adapter passes the contract test: probabilities valid and sum ≈1; deterministic on repeated
input (or variance reported); unknown label rejected; timeout → error (never a guess);
`Truncated` set when window exceeded; `Policy()` honoured by the rule loader.

### 4.6 Labels and corpus

`POST /v1/labels` `{subject, rule?, label, source:"operator"|"outcome", note?, evidence_ref?}` from
the product's review UI. A label snapshots the subject's features and text at that moment into
`corpus_examples` as a JSONL-shaped row `{id, input:{features,text,context}, label, split, source,
meta}`. `abusekit corpus export --split all > corpus.jsonl` produces the file the harness and any
external evaluation framework read. Splits are assigned by hash of `id` (80/20) so they are stable.

### 4.7 Evaluation harness and CI gate

`abusekit eval --dataset corpus.jsonl --rule lure_similarity --scorer jev --out run.json`
→ `run.json = {manifest:{dataset_sha, rule_sha, scorer, model, prompt_version, at}, verdicts:[…],
metrics:{precision, recall, f1, ece, latency_p50, cost_total}}`. `--scorer all` runs every registered
adapter and prints a side-by-side table. `abusekit eval` is also importable (`eval.Run`) and the
`score()` function is exported, so Inspect/promptfoo/Braintrust can drive it without the service.

CI: `make gate` runs the committed corpus through each rule's configured scorer and fails when
precision or recall drops below the floor in `eval/floors.yaml` or ECE exceeds its bound. Prompt
and feature changes are therefore reviewed like code. Shadow verdicts from production are appended
to the corpus only when a label arrives (never unlabelled), so the corpus stays ground truth.

### 4.8 Storage

Tables: `events` (append-only, `(producer,id)` unique, `subject` index, `at` index),
`subjects` (last event, dirty flag), `verdicts` (subject, rule, model, probs jsonb, reason, mode,
scored_at, input_hash), `labels`, `corpus_examples`. Retention: events 90 days, verdicts 1 year,
labels and corpus forever. Same schema on Postgres and SQLite (no jsonb-specific queries in the
hot path). Migrations embedded, applied on start, expand-only.

### 4.9 Ticker

Every 10 s: select dirty subjects (limit 500), extract features over each rule's window, call
`score()`, store verdicts, clear dirty, fire webhook on tier change. Per-adapter concurrency and a
daily cost cap (`cost_cap_usd`) after which that adapter's rules report `unscored:cost_cap`.
Adapter timeouts: 2 s Jev, 5 s Laya (CPU), 15 s Gemini; on timeout the rule is `unscored` this tick
and retried next tick with backoff up to 15 min.

### 4.10 e2a integration (first consumer)

- OSS server: an `abusekit` emitter behind a config block (inert by default, self-host unaffected)
  that emits `subject.created`, `resource.created` (agents, keys), `content.sent` (subject line,
  recipient domain, count, first link host), `content.verdict` (piguard outcome). Emission is a
  buffered goroutine with drop-on-full; never on the request path.
- Billing sidecar: emits `payment.attempt` (from Stripe webhooks and checkout outcomes, with the
  card fingerprint hashed under an abusekit-specific salt) and `subscription.changed`.
- Dashboard/operator: reads the score for the account inspect view; the pause button posts a
  `label`. e2a's policy on the score is e2a's (initially: `high` → operator alert only).
- Backfill: a one-off, operator-run script (kept out of this repo) reads the product's database
  read-only to seed events for a chosen window; abusekit itself never reads a product database.

### 4.11 Alternatives considered

- **Read products' databases instead of push.** Fastest to ship, but couples abusekit to each
  schema, breaks silently on migrations, and cannot serve a second product without a second
  reader. Rejected; kept only as the one-off backfill.
- **Embed scoring in e2a's piguard.** No new service, but the account-level judgement then lives
  in one product, the Gemini key and privacy boundary spread, and the hub/AgentDrive get nothing.
  Rejected. Message-level scoring stays in piguard by design.
- **Let abusekit enforce (hold/pause) directly.** Simpler for e2a today, wrong for every other
  product, and makes the service a policy engine with product-specific semantics. Rejected;
  score-only is the contract.
- **Single scorer (Gemini only).** Fewer adapters, but generative output has uncalibrated
  confidence and no consistency guarantee, and vendor lock. Rejected; the harness exists to make
  the model a measured choice.

## 5. Edge cases and failure handling

- **Adapter down / slow / over cost cap** → rule `unscored`, excluded from tier, retried with
  backoff; the last good verdict is served with `stale:true`. Never a 500, never a guessed score.
- **Malformed or hostile event data** (prompt-injection text in a subject line) → text is passed
  to models only as data inside a structured request; adapters use structured output; Jev cannot
  emit free text at all; the Gemini explainer's output is length-capped and stored as a string,
  never executed or rendered as HTML by abusekit.
- **Duplicate / out-of-order events** → idempotent on `(producer,id)`; features are computed over
  `at`, not arrival order; late events mark the subject dirty again.
- **Subject deleted in the product** → no special handling; events and verdicts persist under the
  opaque subject id (that persistence is the point — see the delete-and-resignup loop). Products
  that need erasure call `DELETE /v1/subjects/{id}` which tombstones events and keeps labelled
  corpus rows with the subject id hashed.
- **Rule config invalid** → reload rejected, previous config stays live, error surfaced on
  `/healthz` and in logs.
- **Batch partially invalid** → per-item codes, valid items accepted.
- **Clock skew** → `at` accepted within ±24 h of server time; otherwise rejected `bad_timestamp`.
- **Label contradicts a verdict** → both stored; the harness reports it; nothing auto-tunes.
- **SQLite deployment under concurrent writes** → single-writer; the ticker and HTTP writes go
  through one serialized writer; documented as laptop/dev only.

## 6. Scalability and extensibility

- Volumes: e2a today is thousands of events/day; the design targets 1M events/day and 100k
  subjects on one Postgres without partitioning; `events` by `(subject, at)` and a `dirty`
  partial index keep the ticker O(dirty).
- Adding a product: a key and, optionally, a features YAML; no code.
- Adding a model: one adapter file + contract test; no changes elsewhere.
- Adding a feature: a function in `feature` registered by name; rules reference it.
- Likely follow-ons made easier: cross-product subject linking (a `links` table keyed by hashed
  identifiers, out of scope now but the opaque-subject design leaves room); a per-message advisory
  endpoint (the pure `score()` already supports single-input scoring); fine-tuning Laya (corpus
  export is the input).
- Deliberately narrow: no plugin loading, no scripting in rules, no per-tenant rule sets in v1.

## 7. Verification strategy

Seams tested (few, at the surfaces callers cross):
1. **HTTP contract**: events (happy path, per-item rejection, HMAC denial, idempotent replay,
   size limit), subject score (shape pinned, `stale`, `unscored` rule), labels, list pagination.
2. **`score()` pure function**: table tests with fixed features and fake scorers; determinism;
   tier derivation; shadow exclusion.
3. **Adapter contract test**: one suite, every adapter, run against fakes in CI and against live
   endpoints in a nightly job with recorded fixtures.
4. **Harness gate**: the incident corpus (redacted) committed; floors set from the first run;
   `make gate` in CI.
5. **Replay test**: the synthetic reference-operator fixture must reach `high` before the first
   `content.sent`, with the default rules and a fake scorer that returns recorded probabilities —
   proves the features and thresholds, independent of a live model.

Manual validation: run the operator backfill against staging, compare the top subjects by score
with the operator's known-abuse list; confirm zero benign synthetic-monitor accounts in `high`.

Likely regressions: feature window arithmetic at day boundaries; calibration drift after a model
update (caught by the gate); a producer sending recipient addresses (caught by ingest redaction
tests).

## 8. Open questions

1. Jev data-handling terms (A1) — needed before any text field goes to Jev; until then Jev rules
   receive features only.
2. Laya CPU latency on the prod VM (A2) — measure on a 2-vCPU box before choosing it for any rule.
3. Cost cap defaults — proposal $5/day per adapter for e2a; confirm.
4. Whether `content.sent` should carry the first 200 chars of body text (better lure recall, more
   sensitive) or only subject line and link host (v1 proposal).
5. Tier cut points (0.4 / 0.8) — placeholders until the first harness run.
6. Licence: Apache-2.0 (decided 2026-09-27); the repo is public.
