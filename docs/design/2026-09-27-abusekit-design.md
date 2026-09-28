# abusekit design — revision 2

Status: revision 2 after dual review (correctness + adversarial), 2026-09-27 · owner: Josh Zhang.
Revision 1 was reviewed and found structurally short in three places: per-subject features could not
see account churn, tiers failed open when a scorer was unavailable, and request signing did not
cover reads or replay. This revision fixes those and tightens every place an implementer would have
had to guess. Changes from r1 are marked **[r2]**.

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

1. A product integrates in under a day with two calls: emit an event, read a score. Measured by the
   first consumer's integration diff being under 300 lines including tests.
2. **[r2]** On the synthetic reference fixtures, with the default rules and the deterministic local
   scorer (no vendor call): (a) the burst fixture (signup, fraud-declined attempts, prepaid success,
   upgrade within minutes, a burst of keys and agents, first sends an hour later) reaches `high`
   within 15 s of the last setup event and before the first `content.sent`; (b) the fast fixture
   (same, with agents and first send inside one minute) reaches `high` on the synchronous
   `evaluate` call the product makes before its first external send; (c) the churn fixture (twenty
   subjects created and deleted in sequence, sharing a signup-email hash or a card fingerprint) has
   every subject from the third onward at `high` on its first `resource.created`.
3. **[r2]** For each registered scorer whose capabilities accept text, the synthetic lure corpus
   scores ≥0.9 risk against a benign corpus of ≥300 examples with ≤2% false positives at the same
   threshold, on a held-out lure family. Reported by the harness with confidence intervals; gated
   in CI on recorded cassettes.
4. Switching a rule's scorer is a config change plus a recorded calibration and gate run; every
   stored verdict records model, checkpoint, render version and calibration id.
5. abusekit being down, slow or over budget never blocks a product request and never reads as
   "benign": emit is fire-and-forget through a durable outbox, reads serve the last verdict with
   `stale:true`, and an unscored subject is `unknown`, never `low`.

## 2. Goals and non-goals

**Goals**
- Score-only: `{score, tier, signals, model}` per subject; the caller owns enforcement.
- Push-based ingestion of typed events; abusekit never reads a product's database.
- **[r2]** Same-product identity linking so churned accounts inherit their predecessors' evidence.
- Model-agnostic: `Scorer` behind a real seam (four adapters), `Explainer` optional; a deterministic
  local scorer that always answers.
- Rules as data (YAML); modes per rule: `shadow` (computed, stored, excluded from tier) and
  `advise` (included). **[r2]** A defined promotion path from shadow to advise.
- Pure `Plan`/`Combine` core with an orchestrator between them, so any evaluation framework can
  drive scoring with recorded results; JSONL corpus; run manifests; harness CLI; CI gate.
- Labels from operator decisions flow back into the corpus under a separate credential.
- One Go binary; Postgres. **[r2]** Importable `pkg/abusekit` client and the pure core; no
  embedded ticker, no SQLite in v1.
- Privacy by construction: static redaction per event type, vendor allowlist tied to terms,
  template-generated reasons, bounded retention with a documented legal basis.

**Non-goals (v1)**
- Enforcement of any kind. Not optional, not pluggable.
- Synchronous scoring of message bodies on a product's send path. Message-level scanning stays in
  the product (e2a: piguard); abusekit consumes its verdicts as events.
- A dashboard. Score endpoint + CLI; operator UI lives in each product.
- Cross-product identity linking. Linking is within one tenant in v1; the link table is designed
  so cross-tenant linking is an additive change.
- Fine-tuning pipelines and payment decisions (Stripe Radar decides payments; abusekit consumes
  outcomes).

## 3. Context, constraints, assumptions

**Existing patterns reused**
- e2a's billing sidecar: private Go service in its own container, one Postgres, config via env.
  abusekit deploys the same way in the hosted compose file. **[r2]** It is reachable only on the
  compose network; nothing is routed through the public proxy.
- e2a's `piguard` engine: `Detector` interface, structured JSON from Gemini, timeouts, explicit
  `Degraded`/`MinOK` handling of missing detectors. abusekit copies the degraded semantics
  (§4.4) rather than dropping failed rules.
- e2a's privacy policy commits screening to the paid Gemini tier; the Gemini adapter is pinned to a
  named billing project (config assertion `gemini.project`), not a key heuristic.

**Assumptions (tracked in §8)**
- A1. Vendor terms for Jev allow redacted numeric features; text fields are withheld from Jev until
  terms covering retention and training are recorded in the adapter allowlist.
- A2. Laya's checkpoint runs in <1 s on a 2-vCPU host for asynchronous jobs; measured before any
  rule selects it.
- A3. Products can emit through a durable outbox (e2a has River) so events survive an abusekit
  outage.
- A4. Eventual scores within 15 s of the last event for a dirty subject at v1 volumes; the
  synchronous `evaluate` endpoint covers the cases where that is too slow.

## 4. Proposed design

### 4.1 Shape and modules

```
product ─emit(events, outbox)─▶ ingest ─▶ store ─▶ worker (ticker, priority queue)
                                                     │  features (+ linked neighbours)
                                                     │  Plan → orchestrator(adapters) → Combine
                                                     ▼
product ◀─GET score / POST evaluate──────── serve ◀── verdicts, subjects.current_tier
```

| Module | Interface | Deletion test |
| --- | --- | --- |
| `ingest` | validate, redact per static schema, link-key extraction, idempotent append | every producer re-implements redaction and idempotency → keep |
| `store` | Postgres repo; events, links, subjects, verdicts, rule_state, labels, corpus | keep |
| `feature` | `Extract(ctx, subject, Neighbors, windows) Features` | every rule re-derives velocity, fan-out, onboarding facts → keep |
| `model` | `Scorer`, `Explainer`, `Capabilities`, registry, calibration | the only vendor seam; 4 scorers → real |
| `core` | `Plan(features, text, rules) []Call` · `Combine(rules, results, calib) Verdict` (both pure) | the test surface for harness and CI → keep |
| `worker` | ticker, priority queue, backoff, budgets, rescore-at, webhooks | the deepest stateful part; **[r2]** its own module, not hidden in `serve` |
| `config` | YAML rules + adapter allowlist, validation, hot reload | **[r2]** `rule` folded in here (it was YAML parsing) |
| `serve` | HTTP handlers, auth, error envelope | shallow by design |
| `eval` | `Run(dataset, rule, scorer) (Manifest, Verdicts)`, metrics, cassettes | keep |

### 4.2 Tenants, producers, subjects, links **[r2]**

- **Tenant** = a product (`e2a`). **Producer** = a component that emits (`e2a-server`,
  `e2a-billing`). Credentials are per producer; subjects are scoped per tenant; idempotency is on
  `(tenant, producer, event_id)`.
- **Subject** = an opaque account id chosen by the tenant. A subject is never deleted by a product's
  own account deletion; the product emits `subject.deleted`.
- **Links** are keyed hashes the tenant sends with events: `email_hash` (normalised: lower-case,
  dots and plus-tags collapsed for gmail-class domains, IDNA), `card_fingerprint_hash`,
  `ip24_hash`, `asn`, `ua_hash`, `device_hash`. Hashes are HMAC-SHA256 under a per-tenant key from
  Secret Manager (rotated by re-hashing links; the raw value is never stored). `links(tenant,
  kind, hash, subject, first_seen, last_seen)` with an index on `(tenant, kind, hash)`.
- **Neighbors(subject)** returns the set of subjects sharing any link key, with a fan-in cap of
  50 per key and a total cap of 200; capped results set `neighbors_truncated=true` (a feature).
  **[S2]** By default, `asn` is excluded from which link kinds count as evidence (already true from
  S1) and `ip24_hash`/`ua_hash` now are too — proven, a shared `ua_hash` alone (the same email
  client or SDK, extremely common) produced `linked_deleted_n=1` for a totally unrelated subject.
  The default candidate set is `{email_hash, card_fingerprint_hash, device_hash}`; all three
  excluded kinds are opt-in per tenant (`internal/feature.Config`). `fingerprint_seen_on_other_subjects`
  always checks `card_fingerprint_hash` specifically regardless of this policy.
- Linked features (all per tenant): `linked_deleted_n`, `linked_labelled_abusive_n`,
  `fingerprint_seen_on_other_subjects`, `neighbors_truncated`. These make the churn fixture
  reachable. **[S2]** `linked_deleted_n` only counts a same-tenant neighbour's PERMANENT
  `subject.deleted` (never a trash-mode one, which e2a's own soft-deletion design allows restoring
  within its retention window — there is no `subject.restored` event in this vocabulary to correct
  a wrongly-flagged restored account, so counting a reversible deletion as abandonment forever was
  the wrong default) and saturates at 3 (an unbounded count let one long churn chain dominate the
  score far past the point it had already proven the pattern). **[S2]** `linked_subjects_n`,
  `linked_max_risk` and `email_hash_seen_on_deleted_subject` remain deferred (not built in v0) —
  see §8's open questions; nothing here currently computes them.
- Cross-tenant linking is an additive change: drop the tenant column from the index key.

### 4.3 Event API

`POST /v1/events`, batch of 1–100, body ≤1 MiB.

**Auth [r2, amended S3]:** `X-Abusekit-Key: <producer key id>`, `X-Abusekit-Timestamp: <RFC3339>`,
`X-Abusekit-Nonce: <hex, ≥16 random bytes>` **[S3]**, `X-Abusekit-Signature: hex(hmac-sha256(secret,
method \n path?query \n timestamp \n key_id \n nonce \n sha256(body)))` **[S3: nonce added to the
signed payload]**. Timestamp within ±5 min; constant-time compare; the same scheme covers every
endpoint including GET (body hash of the empty string). Key scopes: `events`, `labels`, `read`,
`backfill`. A producer key has `events` only; label keys are issued to the operator UI; `backfill`
skips the clock-skew check and never fires webhooks.

**[S3] Nonce and replay window:** the timestamp alone does not prevent replay — a captured request
stays valid to resend for the whole ±5 min it remains "fresh" by the check above. The nonce is a
per-request random value the server remembers per `(key, nonce)` for at least as long as that
request's own timestamp could still independently pass the freshness check: `max(received_at,
timestamp) + 5 min` for an ordinary key. This is `received_at`-anchored rather than a fixed window
from the timestamp alone, because a request signed near the future edge of the ±5 min window (e.g.
timestamp = now+4 min) would otherwise have its replay record expire only 1 minute after the
timestamp itself, while the timestamp remained independently acceptable for a further 4 minutes —
letting the SAME request be replayed again once the record (but not the timestamp's own validity)
had lapsed. A `backfill`-scoped key skips the timestamp-freshness check entirely, so it has no
timestamp-derived bound to anchor a replay record to at all; it instead gets a flat 24 h replay
window from `received_at`, matching this design's own ±24 h backfill-adjacent tolerance elsewhere. A
caller retrying a request (e.g. after a `5xx`) must sign the retry with a FRESH nonce — reusing the
original request's nonce makes a legitimate retry indistinguishable from a replay.

Event: `{id (required, ≤64), subject (≤256), type ([a-z_.]+ ≤64), at (RFC3339 UTC, ±24 h unless
backfill scope), links? {email_hash?, card_fingerprint_hash?, ip24_hash?, asn?, ua_hash?,
device_hash?}, data (object ≤8 KiB post-redaction)}`. `id` is required **[r2]**; a replay with the
same id and identical body is `duplicate`, with a different body `conflict`.

Response `202` `{accepted, duplicates, rejected: [{index, code, message}]}`; codes enumerated
**[r2]**: `bad_id`, `bad_subject`, `bad_type`, `bad_timestamp`, `bad_links`, `too_large`,
`conflict`, `redaction_failed`. Whole-request errors use the envelope
`{error:{code,message,details?}}` with `400 bad_request`, `401 unauthenticated`, `403 forbidden`,
`413 payload_too_large`, `415 unsupported_media_type`, `429 rate_limited` + `Retry-After`.

**Built-in vocabulary** (unknown types stored, available to Go-registered features only):

| Type | data | Notes |
| --- | --- | --- |
| `subject.created` | `channel`, `email_domain_class` (`webmail`\|`corporate`\|`disposable`\|`unknown`), `identity_kind` | **[r2]** email is carried only as `links.email_hash` |
| `subject.deleted` | `mode` (`trash`\|`permanent`) | **[r2]** replaces any DELETE call by products |
| `payment.attempt` | `outcome` (`succeeded`\|`declined`\|`blocked`), `reason`, `funding` (`prepaid`\|`debit`\|`credit`\|`unknown`), `amount_minor`, `currency` | fingerprint travels in `links` |
| `subscription.changed` | `plan`, `status`, `amount_minor` | |
| `resource.created` / `resource.deleted` | `kind`, `name` (≤200, NFKC + confusables skeleton stored alongside), `address_domain` | |
| `content.sent` | `subject_line` (≤200), `recipient_domain`, `recipient_count`, `recipient_hash` (keyed), `recipient_is_own_identity` (bool, product-computed), `first_link_host` | **[r2]** `recipient_hash` + own-identity flag capture rehearsal-to-self |
| `content.verdict` | `source`, `category`, `score` | product-side scanners |
| `subject.class` | `class` (`customer`\|`internal`\|`synthetic`) | **[r2]** internal/synthetic subjects are stored but never scored |

**Redaction [r2]** is a static, versioned schema per event type in code (`ingest/redact.go`):
listed keys pass with their caps; unlisted keys are dropped (not hashed); any value matching an
email address in a field that is not a hash is rejected with `redaction_failed`. Ingest never
consults rules. **[S1]** A listed field's cap truncates an over-cap string value rather than
rejecting the event (truncation is lossy but keeps the event usable; a hard reject for a producer's
minor length overshoot would be disproportionate); a listed field's value must be a scalar
(string/number/bool) — an object or array is a `redaction_failed`, not a silent drop or
stringification. The schema version that produced a given row is recorded on it
(`events.redaction_version`) so a later schema change can identify rows redacted under an older
rule set.

### 4.4 Score API

`GET /v1/subjects/{subject}` → `200` (a seen-but-unscored subject is `200` with `tier:"unknown"`;
`404 not_found` only for a subject never seen in this tenant).

```json
{"subject":"acct_example_1","score":0.93,"tier":"high","degraded":false,"stale":false,
 "events_since_score":0,"scored_at":"2026-10-01T12:01:30Z",
 "signals":[{"rule":"new_account_velocity","risk":0.97,"flagged":true,"mode":"advise",
             "model":"local@1","calibration":"none","reason":"6 resources in 74s; 4 keys in 45s; upgrade 16m after signup; 3 fraud-declined attempts before first success"},
            {"rule":"lure_similarity","risk":0.88,"flagged":true,"mode":"shadow","model":"laya@ck-3","calibration":"cal_7f",
             "reason":"subject lines cluster with a known lure family"},
            {"rule":"llm_review","status":"unscored","error_code":"cost_cap"}]}
```

**Semantics [r2]:**
- Each rule declares `benign_label`; a signal's `risk = 1 − P(benign)` after calibration;
  `flagged = risk ≥ rule.threshold`.
- `score = max(risk)` over scored `advise` rules. `tier` from `score` by global cut points
  (`medium ≥ 0.4`, `high ≥ 0.8`, placeholders until the first gate run). Per-rule thresholds do not
  affect tier; they only set `flagged`.
- `tier = "unknown"` when fewer than `min_scored_advise` (default 1) advise rules are scored, and
  `degraded = true` whenever any advise rule is unscored. Callers must treat `unknown` as
  "no evidence".
- `stale = last_event_at > scored_at`; `events_since_score` counts them. A caller that needs
  freshness calls `evaluate`. **[S1]** The store implements this as `dirty_seq > scored_seq` (both
  monotonic counters bumped/advanced by the same events/scoring rounds this compares) rather than
  literally comparing the two timestamps: two different clocks (the app server that stamps an
  event's `at`, and Postgres's own `now()` for `scored_at`) make a direct timestamp comparison wrong
  under real clock skew, and unconditionally wrong for any test or backfill timestamp far from the
  real wall clock.
- `Cache-Control: no-store`; `ETag` = hash of the verdict ids in the response.

**`POST /v1/subjects/{subject}/evaluate` [r2]** `{deadline_ms ≤ 3000}` → scores the subject now
using rules whose scorer can answer within the deadline (the local scorer always can; vendor
scorers only if their p99 fits), returns the same shape with `evaluated_now:true`. This is what a
product calls before its first external send or a capacity upgrade. Idempotent; rate-limited per
subject (1/s).

**`GET /v1/subjects?tier=&class=&cursor=&limit≤100`** — keyset on `(current_scored_at, subject)`,
at-least-once paging documented; `subjects.current_tier`, `current_score`, `current_verdict_id`,
`current_scored_at` are materialised columns.

**Webhook [r2]:** per-tenant target URL and secret in config; event `subject.tier_changed`
`{tenant, subject, from, to, verdict_id, scored_at}`; same signing scheme; retries 1m, 5m, 30m,
2h, 12h; consumers dedupe on `verdict_id`.

**`DELETE /v1/subjects/{subject}` [r2]** is a legal erasure request, key scope `erase` (operator
only). It destroys event `data` text, verdict reasons and corpus text for the subject, keeps
numeric features and link hashes for subjects with an `abusive` label under a fraud-prevention
legitimate-interest basis for 24 months, and purges everything for other subjects. Products never
call it on account deletion; they emit `subject.deleted`.

### 4.5 Rules (YAML)

```yaml
tiers: {medium: 0.4, high: 0.8}
min_scored_advise: 1
rules:
  - name: new_account_velocity
    mode: advise
    scorer: local            # deterministic; always answers
    inputs: [subject_age_h, resource_velocity_1h, resource_total, key_velocity_1h, key_total,
             upgrade_delay_min, upgraded, declines_before_first_success, first_funding_prepaid,
             name_brand_match, name_has_at, first_day_distinct_domains, self_send_before_external,
             linked_deleted_n, linked_labelled_abusive_n, fingerprint_seen_on_other_subjects,
             neighbors_truncated, burst_ratio_24h_vs_lifetime]
    labels: [benign, suspicious, abusive]
    benign_label: benign
    threshold: 0.6
  - name: new_account_velocity_jev
    mode: shadow
    scorer: jev
    inputs: same_as: new_account_velocity
    labels: [benign, suspicious, abusive]
    benign_label: benign
    threshold: 0.6
    stage: {min_local_risk: 0.3}      # only runs when the local rule is at least 0.3
  - name: lure_similarity
    mode: shadow
    scorer: laya
    text: [subject_line_skeleton, first_link_host]
    labels: [benign, phishing, brand_impersonation, scam]
    benign_label: benign
    threshold: 0.85
    stage: {max_subject_age_h: 168}
```

**Features [r2]:** Go functions registered by name; YAML only references them (no scripting, no
derived features in YAML). Windows are per feature, not per rule: `*_1h`, `*_24h`, `*_total`
(lifetime), and **permanent onboarding facts** (`declines_before_first_success`,
`first_funding_prepaid`, `upgrade_delay_min`, `email_domain_class`) that never age out.
`burst_ratio_24h_vs_lifetime` catches dormant-then-blast. **[S2]** `email_domain_class` remains
deferred — no v0 feature reads it yet; see §8. **[S2]** `upgrade_delay_min` only counts a PAID
`subscription.changed` (`amount_minor > 0`) as an upgrade — a free-plan change or a cancellation
must not read as one. A new `upgraded` (0/1) feature carries whether a paid upgrade has ever
happened at all, independent of the delay's magnitude: `upgrade_delay_min` alone can't distinguish
"just upgraded, delay not meaningfully small yet" from "never upgraded, waited out the clamp
window" below. `subject_age_h` and `upgrade_delay_min` are both clamped at 24h/1440min — proven,
an unbounded value for either let a multi-day-old account swamp the local scorer's linear model
through that feature alone; `burst_ratio_24h_vs_lifetime` is what actually distinguishes "old and
quiet" from "old and just burst," not `subject_age_h`'s raw magnitude. **[S2, round 2]**
`self_send_before_external` and `first_day_distinct_domains` are similarly bounded — proven, an
uncapped count on either let a perfectly benign account (8 self-test emails before ever sending
externally; 30 real customer domains fanned out to on day 1) swamp the model the same way an
unbounded `subject_age_h`/`upgrade_delay_min` did. `self_send_before_external` is hard-capped at 2.
**[S2, round 3]** `first_day_distinct_domains` was ALSO a hard cap (at 10) in round 2, but that made
every count above it read identically — a 30-domain and a 300-domain fan-out scored the same. It is
now a `log1p(n)` curve instead, scaled so `n=10` reproduces exactly the hard cap's old contribution
(no weight change needed) while `n=150` scores meaningfully higher — volume sensitivity above the
old cap is preserved, just compressed rather than flattened to zero.

**Validation at load [r2]:** unknown scorer, unknown feature, labels not accepted by the adapter's
`Capabilities`, text inputs to an adapter whose policy forbids text, `vote(...)` members with
differing label sets, a (rule, scorer) pair with no calibration record and no passing gate run →
the whole reload is rejected and the previous config stays live; `/healthz` reports
`config_error`.

**Promotion shadow → advise [r2]:** a rule may be set to `advise` only if the latest gate run for
its (rule, scorer) meets the floors and the rule has ≥7 days of shadow verdicts with agreement
≥90% against labelled outcomes; `abusekit promote <rule>` checks and records this.

### 4.6 Model layer

```go
type Capabilities struct {
    LabelMode    LabelMode   // Open (any label set) | Fixed(set)
    AcceptsText  bool
    AcceptsFeatures bool
    MaxTokens    int
    Calibrated   bool        // vendor-calibrated; otherwise a calibration map is required
}
type DataPolicy struct{ TermsVersion string; RetainsInputs, TrainsOnInputs bool; AllowsText bool }
type ScoreRequest struct{ Labels []string; Features map[string]float64; Text []string; Context string; RenderVersion string }
type ScoreResult  struct{ Probs map[string]float64; Model, Checkpoint, Render string; LatencyMS int; CostMicro int64; Truncated bool }
type Scorer    interface{ Name() string; Capabilities() Capabilities; Policy() DataPolicy; Score(context.Context, ScoreRequest) (ScoreResult, error) }
type Explainer interface{ Name() string; Policy() DataPolicy; Explain(context.Context, ScoreRequest, ScoreResult) (string, error) }
```

Adapters in v1 **[r2]**:
- **local** — deterministic logistic model over registered features with hand-set weights in
  `config/local_weights.yaml`; no network, no cost; always registered; the advise rule of last
  resort. Its weights are reviewed like code and gated by the harness like any scorer. **[S1]**
  Non-benign probability mass is split evenly across every other requested label (only `risk = 1 -
  P(benign)` is ever read off a signal); the harness's per-label precision/recall is therefore not
  meaningful for local specifically — only its binary `flagged` metrics are.
- **jev** — hosted typed decisions; `LabelMode: Open`, `Calibrated: true` (verified by the harness,
  not assumed); features are rendered to text by a versioned template (`Render`); text inputs are
  refused until `Policy().AllowsText` is set from recorded terms.
- **laya** — local weights; `LabelMode: Fixed(checkpoint set)`; `AcceptsFeatures: false` (text
  only); 512-token window with `Truncated` reported; requires a calibration map.
- **gemini** — structured JSON; `Scorer` and `Explainer`; `Calibrated: false` (map required);
  pinned to a named paid billing project.
- **vote(a,b,…)** — mean of member risks after calibration; loader requires identical label sets.

**Calibration [r2]:** a map per (rule, scorer, checkpoint) fitted by
`abusekit calibrate --rule R --scorer S` (Platt or isotonic on the labelled corpus), identified by
`cal_<hash>` in every verdict and manifest. Rules refuse to run a scorer without a current map
unless `Calibrated: true`.

**Explainer [r2]:** `reason` on every signal is generated from a deterministic template over
feature values (never from text). An LLM explanation is opt-in per rule, stored separately as
`llm_reason` with `untrusted: true`, receives features and probabilities only (never raw text),
and is documented as plain text that products must not render as HTML. Text from events shown to
operators is labelled as quoted untrusted input in the product UI.

**Adapter allowlist [r2]:** `config/vendors.yaml` lists each adapter with `terms_version`,
`dpa_ref` and the `DataPolicy`; the loader refuses an adapter absent from the list.

**Contract test:** every adapter, every CI run, against fakes; nightly against live endpoints with
recorded cassettes refreshed. Asserts: valid probabilities summing to 1±0.01; determinism or
reported variance; unknown label rejected; timeout → error; `Truncated` on overflow;
`Capabilities` honoured; `Policy` honoured by the loader.

### 4.7 Core: Plan / orchestrate / Combine **[r2]**

`core.Plan(features, text, rules) []Call` decides, purely, which (rule, scorer, request) calls to
make, applying `stage` conditions and `input_hash` skipping (unchanged inputs reuse the stored
verdict). The worker or the harness executes calls through adapters (or replays recorded results).
`core.Combine(rules, results, calibration) Verdict` reduces, purely, to risks, flags, score, tier,
`degraded`. The harness and CI drive `Plan` and `Combine` with recorded results; only the nightly
job touches vendors. **[S1]** The caller supplies each rule's prior-round state (last input hash,
last calibrated risk) as plain data on `RuleState` rather than `Plan` reaching into a store itself;
one consequence is that a `stage: {min_local_risk: ...}` condition gates on the risk from the
*previous* scoring round, not the one currently being planned, so a staged rule's activation lags
the gating rule's own by one round. The input hash also covers the scorer's own version/checkpoint,
the render template version, the calibration id in effect, and the rule's `benign_label` — not just
the resolved features/text/labels — so a scorer upgrade, a newly fitted calibration, or a
`benign_label` config change always forces a rescore rather than reusing a stale verdict.

### 4.8 Worker **[r2]**

- **Queue:** `subjects.dirty_seq` (monotonic, set on every event) and `scored_seq`; the worker
  selects `dirty_seq > scored_seq OR next_rescore_at <= now`, ordered by priority: dirty subjects
  strictly ahead of timer-only rescores **[S2]**, then new subjects (age < 24 h), then higher
  `current_score` with a never-scored subject (`current_score IS NULL`) ranked highest of all
  **[S2]** (the design's original "rising `current_score`" wording under-specified the NULL case;
  an account with no evidence yet is the most urgent to get a first score, not the least), then
  oldest dirty; `FOR UPDATE SKIP LOCKED`; batch 200 per 10 s tick; multi-instance safe. **[S2]**
  Multi-instance safety needed a real per-subject claim lease (`subjects.claimed_until`) on top of
  the row lock — proven, a bare select-then-commit-before-scoring claim let two instances issue 40
  scorer calls for 20 subjects, since the lock's own duration was too short to cover a whole scoring
  pass.
- **Compare-and-clear:** after scoring, set `scored_seq = the dirty_seq read at start`; a later
  event keeps the subject dirty.
- **Rescore-at:** `next_rescore_at` = earliest feature-window expiry, so decayed velocities are
  recomputed without a new event. **[S2]** Also the minimum of: the earliest in-flight rule's
  `retry_at`, and the next UTC midnight if any rule was `cost_cap`'d this round — proven gaps, an
  errored rule on an otherwise-quiet subject was never retried, and a cost-capped rule was never
  retried once the day rolled over. Window-exit candidates only consider event types a windowed
  feature actually reads, and are coalesced to 5-minute buckets — proven, an uncoalesced burst of
  60 sends in quick succession scheduled 60 nearly-simultaneous separate rescores instead of one
  shared one. **[S2, round 2]** Accepted tradeoff: for a genuinely future-dated event (a backfill or
  a clock-skewed producer, §5's ±24 h allowance), this same coalescing can add up to 5 minutes of
  delay after the burst it's tracking actually lands, on top of the event's own timestamp — e.g. an
  event dated exactly on a 5-minute boundary schedules its rescore for the boundary AFTER that (never
  the event's own instant), and a slightly-later event in the same burst coalesces onto whichever
  boundary is next, not necessarily the nearest one to when it individually arrives. Scheduling the
  exact enter time instead (dropping coalescing for future-dated candidates specifically) would
  reintroduce a version of the original "60 nearly-simultaneous rescores" problem for a producer that
  backfills many future-dated events at once, for a bound (≤5 minutes, once) that's already small
  relative to the 1 h/24 h windows every rate feature reads. Left as-is.
- **Rule state:** `rule_state(subject, rule, attempts, retry_at, last_error)`; backoff 30 s, 2 m,
  5 m, capped at 15 m; a subject with any advise rule in backoff serves `degraded:true`. **[S2]** A
  whole-pass failure (the extractor or a store call erroring, as opposed to one rule's scorer)
  backs the SUBJECT off the same way (`subjects.fail_count`/`next_attempt_at`) — proven, a subject
  whose scoring always failed outright was reclaimed and retried every single tick forever,
  starving the rest of the batch.
- **Budgets:** per adapter daily cap, per subject daily cap (default 20 calls), per producer daily
  cap; 25% of each adapter cap is reserved for subjects at `medium` or above; hitting a cap pages
  the operator and sets `error_code: cost_cap` on the affected rules while the local rule keeps
  answering. **[S1]** Implemented per-TENANT rather than per-producer (a subject isn't tied to one
  producer once claimed off the dirty queue); **[S2]** counters are persisted (a `budget_usage`
  table), not in-memory-only, so a restart or a second worker instance shares the same count
  instead of each keeping its own.
- **Class skip:** `internal` and `synthetic` subjects are stored but never queued.
- **Metrics [S2]:** queue depth, oldest dirty age, per-adapter calls/errors/mean-latency, verdicts
  by tier, and budget denials are implemented (`internal/worker.Metrics`, expvar-backed);
  `emit_dropped_total` is the producer-side emitter's own counter (S6, not yet built).

### 4.9 Labels and corpus **[r2]**

`POST /v1/labels` (key scope `labels`, operator identity required):
`{subject, rule?, label, source: "operator"|"outcome", actor, note? (≤500, retained like events),
evidence_ref?}` → `201`. Label vocabulary per rule is the rule's `labels`; with `rule` omitted the
label applies to the subject-level `benign|abusive`.

A label writes a **corpus example** that stores the redacted event slice up to `decision_at`
(default: the first `content.sent` after signup, else the label time) plus the features as
extracted at that time. Features are re-extracted at eval time from the slice, so feature-code
changes are testable; the stored features serve external tools. Splits are by link cluster (fallback
subject) hashed 80/20, and the harness can hold out a whole lure family. A label enters the
**gate** corpus only after a second source (a second operator, or an `outcome` label) agrees.

`abusekit corpus export --split all|train|test --schema corpus-v1.json > corpus.jsonl` with a
published JSON Schema.

### 4.10 Evaluation harness and CI gate **[r2]**

- `abusekit eval --dataset corpus.jsonl --rule R --scorer S|all --cassettes dir --out run.json`.
  Manifest: abusekit version and git sha, dataset sha, split, rule sha, scorer, model, checkpoint,
  render version, calibration id, adapter parameters, label set and thresholds, counts of unscored
  and truncated, timestamp. Metrics: per-label precision/recall/F1 at the rule threshold, binary
  precision/recall on `flagged`, ECE with 10 bins, latency p50/p95, cost; unscored counts as a
  miss. Wilson intervals on every rate.
- External frameworks: `abusekit score --jsonl` (stdin→stdout, no persistence),
  `POST /v1/score:dryrun` (same, over HTTP), the JSON Schema for corpus rows, and the exported Go
  `core.Plan`/`core.Combine`.
- PR CI (`make gate`): recorded cassettes keyed by `(model, checkpoint, render, input_hash)`; no
  secrets, no spend; fails below `eval/floors.yaml` (floor = lower interval bound of the reference
  run) or above the ECE bound. Nightly: live adapters with tolerance bands, cassette refresh.
- The committed corpus is synthetic (lures written in the style of the incident families, `.test`
  domains, shifted timelines, fictional ids). The real incident corpus lives in private storage
  and feeds only the nightly job via a secret.

### 4.11 Storage

Postgres only in v1. Tables: `events` (append-only; unique `(tenant, producer, id)`; indexes
`(tenant, subject, at)`), `links`, `subjects` (dirty_seq, scored_seq, next_rescore_at, class,
current_* columns), `verdicts` (subject, rule, model, checkpoint, render, calibration, probs
JSON text, risk, flagged, mode, reason, llm_reason, input_hash, scored_at), `rule_state`,
`labels`, `corpus_examples`, `calibrations`. Retention: events 90 d (text fields), 24 months
(numeric + links) for abusive-labelled subjects, verdicts 12 months, labels/corpus per §4.4
erasure rules. Migrations embedded, expand-only.

### 4.12 First consumer: e2a

- OSS server: an inert-by-default `abusekit` emitter behind a config block, writing events to
  River (durable outbox) and draining to abusekit with retries; emits `subject.created` (with
  `email_hash` and `email_domain_class`), `subject.deleted`, `resource.created/deleted` (agents,
  keys), `content.sent` (subject line skeleton, recipient domain, hash, own-identity flag, first
  link host), `content.verdict`, `subject.class` for prober/monitor accounts. Counter
  `abusekit_emit_dropped_total` alerts on any drop.
- Billing sidecar: `payment.attempt` (outcome, reason, funding, `card_fingerprint_hash` under the
  tenant key) and `subscription.changed`.
- Dashboard: the account inspect view reads the score; the pause action posts a label under an
  operator key. e2a's initial policy: `high` → operator alert; `evaluate` before first external
  send and before capacity upgrade, with `unknown` treated as "wait for the score".
- Backfill: an operator-run script outside this repo, using a `backfill`-scope key.
- Privacy page: subprocessor list updated before any vendor scorer leaves shadow.

### 4.13 Alternatives considered

- Read products' databases: fastest, couples to schemas, one product only. Rejected.
- Embed in piguard: no service, but one product only and the privacy boundary spreads. Rejected;
  message-level scanning stays in piguard.
- Let abusekit enforce: simpler for e2a today, wrong for every other product. Rejected.
- Single vendor scorer: uncalibrated confidence and lock-in; the harness exists to make the model a
  measured choice. Rejected.
- **[r2]** SQLite and embedded mode in v1: doubled the test matrix for no consumer. Deferred; the
  pure core and the client package are what a library user needs.

## 5. Edge cases and failure handling

- Adapter down / slow / capped → rule `unscored` with a code; `degraded:true`; local rule still
  answers; tier never becomes `low` by absence.
- Flood of cheap events from many subjects → per-producer and per-subject budgets, reserved
  headroom for elevated subjects, staged vendor rules; the local rule is unaffected.
- Hostile text in names or subject lines → never reaches the explainer; scorers receive it only as
  data in structured requests; a text rule alone cannot raise `score` above `medium` unless a
  feature rule is ≥ `medium` (config `text_rules_need_feature_support: true`). **[S1]** "text rule"
  means any rule declaring `text` at all, even one that also declares `inputs` — a trivial/permanent
  onboarding-fact input (e.g. `email_domain_class`) alongside `text` does not exempt a rule from this
  cap, and such a rule does not itself count as the feature-only evidence another text rule needs.
- Duplicate / out-of-order / late events → idempotent ids; features over `at`, with `first_seen_at`/
  `first_seen` taking `LEAST(existing, new)` so out-of-order delivery can't leave first-seen at a
  later time than the true earliest event; late events bump `dirty_seq`.
- Account churn → links carry evidence across subjects; `subject.deleted` is a feature, not an
  erasure.
- Homoglyph names → NFKC + confusables skeleton before brand matching. **[S1]** The confusables table
  is a curated subset (Cyrillic/Greek Latin-lookalikes plus common leetspeak digit substitutions)
  covering lookalikes seen in labelled examples, not a full UTS #39 confusables implementation.
  **[S2]** Brand matching itself moved to `config/brands.yaml` (data, not a Go literal) and is now
  word/token-boundary-aware rather than a bare substring check — proven, a substring check both
  false-positived ("Pineapple"/"Grapple"/"Applebee's"/"Amazonas"/"Striped" all matching a bare
  brand-name substring) and false-negatived (a multi-word brand like "Wells Fargo" never matching
  its own smashed-together "wellsfargo" dictionary entry, since a real display name has a space the
  entry didn't). Generic single-word brands that collide with legitimate integration names
  ("Stripe Webhook Relay", "Google Calendar Sync", "Microsoft Teams Relay") are excluded from the
  shipped list rather than flagged and accepted as noisy — word-boundary matching alone can't tell
  "impersonating Stripe" from "a real Stripe integration named after Stripe"; proper context-aware
  matching for those is future work.
- Invalid config → reload rejected, previous config live, `/healthz` reports it.
- Lost update in the worker → `dirty_seq` compare-and-clear.
- Clock skew → ±24 h on events (except `backfill` scope), ±5 min on request signatures.
- Label contradicting a verdict → both stored, reported by the harness, nothing auto-tunes.

## 6. Scalability and extensibility

- Targets 1M events/day, 100k subjects, one Postgres; the queue is O(dirty); `links` lookups are
  index hits with fan-in caps.
- Add a product: keys, `subject.class` tagging, an outbox. No code.
- Add a model: one adapter + contract test + allowlist entry + calibration run.
- Add a feature: one Go function; rules reference it.
- Made easier later: cross-tenant links (drop a column from an index), per-message advisory
  scoring (`score --jsonl` already exists), Laya fine-tuning (corpus export is the input),
  SQLite/embedded (core is pure).

## 7. Verification strategy

1. HTTP contract tests: events (happy, per-item codes, conflict vs duplicate, signature on GET,
   replay rejection, skew, size), score shape pinned including `unknown`/`degraded`/`stale`,
   evaluate deadline behaviour, labels (scope denial), list paging, erasure semantics.
2. `core.Plan`/`Combine` table tests: staging, input-hash skipping, reduction, tier, degraded.
3. Adapter contract suite against fakes in CI, live nightly.
4. Feature tests: windows at day boundaries, permanent facts, burst ratio, neighbours with
   truncation.
5. Replay fixtures (synthetic): burst, fast, churn (§1 criterion 2) with the local scorer and with
   recorded vendor results.
6. Gate: synthetic corpus, held-out family, cassettes, floors with intervals.
7. Manual: staging backfill; top subjects vs the operator's known-abuse list; zero `internal` or
   `synthetic` subjects scored.

## 8. Open questions

1. Jev terms (A1): text stays withheld until recorded in `vendors.yaml`.
2. Laya CPU latency on the hosted VM (A2): measure before any rule selects it.
3. Budget defaults: $5/day per vendor adapter, 20 calls/subject/day, 2,000/producer/day. Confirm.
4. Whether `content.sent` should carry the first 200 chars of body text. v1 proposal: no.
5. Tier cut points and the local scorer's initial weights: placeholders until the first gate run.
6. Legal review of the 24-month retention basis and the privacy-page subprocessor update.
7. **[S2]** Deferred v0 features, not yet computed by anything: `linked_subjects_n` (§4.2's total
   same-tenant neighbour count, distinct from the deleted/labelled/fingerprint-specific ones that
   are built), `linked_max_risk` (§4.2, needs a neighbour's own current score, not just its
   deleted/labelled status), `email_hash_seen_on_deleted_subject` (§4.2, narrower than
   `linked_deleted_n`: specifically whether THIS subject's own email hash, not device or card, was
   seen on a deleted one), and `email_domain_class` (§4.5, `subject.created`'s own field is ingested
   and stored but no rule reads it as a feature yet). None are wired into `config/rules.yaml`'s
   `new_account_velocity` inputs or `internal/feature.Names`; adding any is a small, additive change
   whenever a labelled corpus justifies the weight.
8. **[S2]** Whether ASN should ever count as same-tenant linking evidence at all (§4.2) is still
   open — defaulted to excluded (alongside `ip24_hash`/`ua_hash`, also newly excluded by default)
   pending real labelled data; see `internal/feature.Config`'s own doc comment. Decision owner:
   Josh.
9. **[S2, round 3, D3]** Known limits of the local scorer's v0 feature set, deliberately deferred
   rather than fixed here (S4/S5 concerns — the eval harness and a labelled-corpus retune are what
   would justify each one, not another hand-tuned weight):
   - `fingerprint_seen_on_other_subjects` only checks the `card_fingerprint_hash` link kind (§4.2) —
     a churn chain sharing a `device_hash` but never a card (no payment method reused, or none
     collected yet) gets none of this signal from its own onboarding round; `linked_deleted_n`
     still catches it, but only from the round AFTER a predecessor is deleted, i.e. one subject
     later than a card-linked chain would.
   - No v0 feature reacts to a fan-out that happens AFTER day 1. `first_day_distinct_domains` is
     anchored to `firstSeenAt` and permanently fixed once that window closes (§4.5);
     `burst_ratio_24h_vs_lifetime` only compares recent activity to the subject's OWN lifetime
     total, so a long-quiet account's day-30 blast is caught by burst detection generally, but
     nothing specifically measures recipient fan-out breadth past the first day the way
     `first_day_distinct_domains` does within it.
   - No feature represents a "verified owner" signal (a human confirming control of the account —
     email verification, a KYC-style check, a long-lived OAuth session) that would legitimately
     lower risk independent of behavioural velocity. Every v0 signal is behavioural; an
     otherwise-suspicious-looking but genuinely verified account has no way to net that out.
