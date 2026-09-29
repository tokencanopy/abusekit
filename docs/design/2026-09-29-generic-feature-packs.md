# Generic feature packs, product vocabularies, and declarative custom features

Status: proposed, revision 2 (after adversarial review), 2026-09-29 · owner: Josh Zhang · amends
[`2026-09-27-abusekit-design.md`](2026-09-27-abusekit-design.md) §4.2, §4.3, §4.5, §4.6, §4.8 and
§4.10. Written against `main` at S3, with the two open PRs treated as merged: #5 (S4 evaluation
harness) and #7 (S2b send-volume, webmail, recipient and subject-brand features). `main §x` refers
to the main design. §13 lists what changed from revision 1.

## 1. Problem statement

abusekit's current feature set assumes an email platform. The main design promises scoring for
any product that mints accounts, takes payments and lets users create resources. The built code
falls short of that:

- **The vocabulary is email-shaped.** `content.sent` carries `subject_line`, `recipient_domain` and
  `recipient_is_own_identity`. Resource kinds are an undeclared convention: `internal/feature`
  counts `kind == "key"` (plus #7's spelling aliases) and treats every other kind as generic.
- **Much of the feature set is email-specific.** Nine of the 25 features on `main` + #7 are email
  features, and two more assume API keys. A file-sharing, payments, chat, developer-API or
  AI-inference product would get these at zero.
- **Custom event types are dead weight.** Main §4.3 says unknown types are stored "for
  Go-registered features only". No such feature exists, so the only way to add a signal is to
  write Go in this repo.
- **Everything is global and flat.** There is one `rules.yaml`, one `local_weights.yaml`, one
  feature namespace, and one subject kind: the account.

**Desired outcome.** A product that is not an email platform onboards with a private YAML profile.
In that profile it:
- declares its event types, field kinds, link kinds and subject kinds;
- enables the packs that fit its domain;
- defines its product-specific signals as declarative features;
- runs them in shadow.

Every new value is pseudonymised or dropped at ingest. For e2a, feature values, tiers and rescore
times stay identical, and risk moves by no more than a stated floating-point bound.

### Success criteria (measurable)

1. **Semantically identical migration.** A golden replay covers every committed fixture: all of
   `eval/fixtures/*.jsonl` (including #7's) and the synthetic corpus. It scores after every event
   and at every scheduled rescore instant. Across the rename and every later slice:
   - every feature value is identical as `math.Float64bits`, compared under the rename map;
   - every `NextRescoreAt` and every tier is identical;
   - every risk and score satisfies `|Δ| ≤ 1e-12` (§5.2 derives the bound).

   Input hashes, cassette keys, the `run.json` rule/weights SHAs and the local `Version()` may
   change **only in the rename slice**. The golden records the before and after values.
2. **Five generic scenarios.** Each scenario in §7 loads as a fictional profile, with zero Go
   changes for everything §7 marks as declarative. Held-out fixtures are authored after the
   features and never used while writing them. On those fixtures, with uniform priors (§5.9),
   each scenario's abusive fixtures outrank every one of its benign fixtures.
3. **Enablement is enforced.** For a tenant without a pack, none of that pack's features is
   computed, stored or rendered. A rule that references one fails to load with
   `feature_not_enabled`.
4. **The DSL is correct and bounded.**
   - Every operation equals a naive reference implementation on 10,000 randomized histories,
     including shuffled arrival, ties and future-dated events.
   - Per-subject time is measured **end to end**: store load, JSON decode, view projection, pack
     extraction and orchestration. For a profile at the §5.7 limits with history at its byte cap,
     it is p99 ≤ 50 ms on the reference 2-vCPU host.
   - The step budget is calibrated from that benchmark (§5.7), so the budget cuts off work before
     the latency target is breached.
5. **Privacy by construction.** Property tests show that, at rest:
   - no stored value matches an email, card (Luhn), IP or phone shape outside a masked `text`
     field;
   - every stored `hash` value is an abusekit-keyed HMAC;
   - undeclared data keeps only type, time and field names.

   The loader fuzzer finds no profile that passes validation while breaking a §5.7 limit.
6. **Flooding doesn't evade.** For every built-in and DSL feature, a property test adds cheap
   events totalling up to 10 times the byte cap to an abusive fixture. Risk must not fall (§5.7).

## 2. Goals and non-goals

**Goals**
- Rename the built-in features into namespaced names **once**, while no production verdicts or
  vendor cassettes exist. There is no compatibility bridge (§5.2).
- Per-tenant profiles, kept in a private config mount. Only fictional example profiles live in
  this repo.
- Product-declared vocabularies:
  - custom event types, with a field kind and optional role for each field;
  - extension fields on built-in types;
  - an `activity` role for types;
  - resource-kind roles;
  - declared link kinds;
  - subject kinds beyond `account`.
- Redaction:
  - pseudonymise every declared hash with an abusekit-held key;
  - drop every undeclared value;
  - validate domains against the public suffix list (PSL);
  - mask text or store it as a skeleton;
  - scan for card, IP and phone shapes.
- A closed declarative feature DSL covering the five scenarios in §7. Every feature has a
  mandatory cap, deterministic semantics and a deterministic step budget.
- Packs (`core`, `email`, `brand`) enabled per tenant, each with starter weights, fixtures, floors
  and a `packtest` harness. Uniform priors, shadow-only, until a tenant has labels.

**Non-goals**
- Code or expressions in config: no CEL, regex, WASM or plugins (§5.12).
- Weight fitting (§12 Q8) and cross-tenant linking (main §2).
- Changes to `Plan`/`Combine` math, apart from the stage-gate fix (§5.8) and per-tenant rule sets.
- Runtime vocabulary declaration over HTTP (§5.11).
- A neutral built-in "delivery" event. Revision 1 proposed `delivery.sent`; revision 2 drops it
  (§5.4, §13).

## 3. Relevant context and constraints

**Code this design touches (`main` + #5 + #7):**
- `internal/event`:
  - `Validate`, and a static `schema` for redaction (`RedactionSchemaVersion` is 2 after #7).
  - Unknown types keep every key after a leak scan; unlisted keys of known types are dropped.
- `internal/feature`:
  - a monolithic `Extract` returns a fixed 25-field `Features`, with `Map()` and `Names`;
  - `nextRescoreAt` hard-codes `resource.created` and `content.sent`;
  - `firstSeenAt` is the minimum event `at`.
- `internal/core`:
  - `inputHash` JSON-encodes the rule's features keyed by name;
  - `quantizeAgeFeaturesForHash` switches on the literals `subject_age_h` and `upgrade_delay_min`;
  - `stageSkip` reads the literal `features["subject_age_h"]`;
  - `maxRiskByScorer` takes the maximum over **every** local rule, shadow rules included.
- `internal/model/local`: sums in sorted feature-name order, and `Version()` hashes the whole
  JSON-encoded `Weights` struct.
- `internal/worker`:
  - `renderReason` prints flat feature names into every stored verdict reason;
  - `computeVerdict` builds `currentRuleNames` from a global config.
- `internal/serve`: `currentRuleNames()` reads the global config.
- `internal/store`:
  - `EventsForSubject` loads every event, ordered by `(at, seq)`;
  - `corpus_examples.features` is JSON keyed by feature name;
  - `subjects.first_seen_at` keeps `LEAST(existing, new)`.
- `eval` (#5):
  - `hashScoreRequest` keys cassettes over `req.Features` by name;
  - `run.json` records `dataset_sha`, `rule_sha` and `weights_sha`;
  - corpus-v1 rows key features by name.

**Assumptions** (unconfirmed ones repeat in §12):
- A1. S5 (vendor adapters) and S8 (hosted deploy) have not shipped. No production verdicts, corpus
  rows or vendor cassettes exist yet. That is what makes a one-time rename safe, and why the rename
  slice must land before S5 and S8.
- A2. At most about 100 tenants and at most 64 custom features per tenant. Event text retention is
  90 days (main §4.11).
- A3. Producers can compute a keyed hash of any identifier they want counted. abusekit re-hashes
  it anyway (§5.6), so a producer's mistake can't turn into stored personal data.

## 4. Proposed design: overview

```
private config mount: tenants/<tenant>/profile.yaml + history/NNNN.yaml (append-only, CI-checked)
  packs · vocabulary (types, fields+kinds+roles, extensions, link kinds, subject kinds)
  features (custom.*) · rules · weights
                     │ compile + validate per tenant (atomic, isolated)
                     ▼
ingest ─▶ vocab.Redact(tenant): leak scan · kind rules · re-HMAC hashes · drop undeclared
       ─▶ store: event row + vocab_version + (subject_kind, subject) index rows (≤ 4 per event)
worker (per-tenant fair queue) ─▶ loader: time-bounded + onboarding-full + byte cap
       ─▶ feature.Extract(profile): core · email · brand (Go) · custom (compiled DSL), under a
          deterministic step budget
       ─▶ core.Plan / Combine (math unchanged)
```

| Module | Interface | Deletion test |
| --- | --- | --- |
| `internal/vocab` (new) | `Compile(builtin, decl) (*Vocabulary, error)`; `(*Vocabulary).Redact(*event.Event, Keys) error` | Without it, redaction, kinds, roles and extensions spread across ingest and the packs. Keep. |
| `internal/pack` (new) | `Pack` interface + registry; adapters `core`, `email`, `brand` (Go) and `custom` (compiled DSL) | Four adapters, so the seam is real. Without it, `feature.Extract` stays a monolith. |
| `internal/pack/custom` (new) | `Compile(tenant, []Def, *Vocabulary, CostTable) (Pack, error)` | Holds every DSL semantic. Keep. |
| `internal/feature` | `Extract(ctx, profile, subject, History, now) (Result, error)` | A thin orchestrator. The worker, evaluate and eval all call this one function. |
| `internal/store` | `LoadHistory(ctx, tenant, subjectRef, LoadPlan) (History, error)` replaces `EventsForSubject` for scoring | Bounded loading lives in one place. |
| `internal/secret` (new) | `Keys` interface (§5.6), provider-agnostic | Two adapters (a file, a cloud secret manager). Keep. |
| `internal/config` | `LoadProfiles(mount, deps) (map[tenant]*Profile, map[tenant][]error)` | Absorbs `rules.yaml`. Per-tenant atomic reload. |

## 5. Proposed design: detail

### 5.1 Packs: registration, enablement, dependencies

```go
type Pack interface {
    ID() ID                     // {Name: "email", Version: 1}
    Requires() []string         // e.g. email@1 → [core]
    Features() []FeatureDef
    // Extract is pure: same input → same output, whatever the arrival order. Every step it
    // takes is charged to in.Budget (§5.7).
    Extract(ctx context.Context, in Input) (Output, error)
}

type FeatureDef struct {
    Name          string   // "email.sends_10m_max"; grammar §5.3
    HashQuantum   float64  // input-hash bucket (replaces the name switch in core)
    Bound         float64  // max value; used by uniform priors (§5.9)
    PriorSign     int8     // +1 / -1; uniform-prior and golden-sign default
    TruncationDir int8     // -1 if the value can only fall when history is truncated, else 0/+1 (§5.7)
    Reads         []string // types/roles read: dispatch, load plan, rescore scheduling
    Lookback      Lookback // history the feature needs (§5.7 load plan)
    RequiresPacks []string // e.g. email.subject_brand_match → [brand]
    SubjectKinds  []string // kinds this feature is defined for; default [account]
}

type Input struct {
    Tenant    string
    Subject   SubjectRef       // {Kind, ID}
    History   History          // bounded, ordered (at, producer, id); truncation flags
    Now       time.Time
    Start     time.Time        // subject.created.account_created_at, else subjects.first_seen_at
    Neighbors NeighborResolver // store-backed, budgeted, cached per extraction
    Params    PackParams
    Budget    *StepBudget
}

type Output struct {
    Values    map[string]float64
    Rescore   []time.Time
    Truncated bool            // this pack's budget or the history's truncation was hit
}
```

**Enablement.** A profile lists `packs: [core@1, email@1, brand@1]`. The rules:
- `core` is always enabled; every other pack is opt-in.
- A pin names a major version, and a pack's semantics change only by shipping a new major version.
- `Requires` must hold, or the profile fails with `pack_requires`.
- A feature whose `RequiresPacks` or `SubjectKinds` don't match is not registered for that tenant
  or kind.
- Packs run in the fixed order `core`, `email`, `brand`, `custom`. Each has its own namespace, so
  two packs can't produce the same feature name.

**Pack contents after the one-time rename:**

| Pack | Features (old flat name → new name) |
| --- | --- |
| `core@1`, account | `subject_age_h`→`core.subject_age_h` |
| `core@1`, payment | `upgrade_delay_min`→`core.upgrade_delay_min`, `upgraded`→`core.upgraded`, `declines_before_first_success`→`core.declines_before_first_success`, `first_funding_prepaid`→`core.first_funding_prepaid`, `fingerprint_seen_on_other_subjects`→`core.fingerprint_seen_on_other_subjects` |
| `core@1`, resource velocity | `resource_velocity_1h`→`core.resource_velocity_1h`, `resource_total`→`core.resource_total`, `key_velocity_1h`→`core.credential_velocity_1h`, `key_total`→`core.credential_total` |
| `core@1`, linked subjects | `linked_deleted_n`→`core.linked_deleted_n`, `neighbors_truncated`→`core.neighbors_truncated` |
| `core@1`, labels | `linked_labelled_abusive_n`→`core.linked_labelled_abusive_n` |
| `core@1`, behaviour change | `burst_ratio_24h_vs_lifetime`→`core.burst_ratio_24h_vs_lifetime` (counts `resource.created` + `content.sent` + declared `activity` types, §5.4); new `core.history_truncated` (§5.7) |
| `email@1`, requires `core` | `sends_10m_max`, `sends_1h`, `sends_first_day`, `webmail_recipient_share`, `webmail_sends_1h`, `distinct_recipients_1h`, `first_day_distinct_domains`, `self_send_before_external`, `subject_brand_match` (also requires `brand`), each becoming `email.<same>`. The pack owns the built-in `content.sent` type. |
| `brand@1` | `name_brand_match`→`brand.name_match`, `name_has_at`→`brand.name_has_at`; new `brand.title_match` over any declared field with `role: title` (§5.4) |

In the early slices every built-in feature is available to every tenant (P2, §9). Pack **gating**
comes later, in P5.

New `core` features are additive and are not in e2a's rule:
- `core.email_domain_class_disposable` (deferred in main §8 Q7);
- `core.verdict_max_24h`;
- `core.history_truncated`.

### 5.2 The one-time rename (replaces revision 1's canonical-key bridge)

**Decision.** Rename in one slice (P1) and re-baseline the golden replay once. No alias table
survives past that slice. The bridge existed only to keep stored verdict hashes and vendor
cassettes stable. Under A1 there are none, and a bridge would keep two names for everything alive
forever.

**Every consumer the rename touches, with its test:**

| Consumer | Change | Test |
| --- | --- | --- |
| `core.stageSkip`, which reads the literal `features["subject_age_h"]` | Reads `core.subject_age_h` instead. The `Rule.Stage` **keys** (`max_subject_age_h`, `min_subject_age_h`) are stage names, not feature names, and keep their names. | Table test: a staged rule skips at the age bounds with a namespaced vector. A grep test fails if any `core` source still contains a flat feature literal. |
| `core.quantizeAgeFeaturesForHash`, which switches on names | Deleted. Quantization comes from `FeatureDef.HashQuantum` (1 h for `core.subject_age_h`, 60 min for `core.upgrade_delay_min`), passed to `Plan` in `core.Vector`. | The existing "hash stable under sub-hour drift" tests, re-run with namespaced names. |
| `core.inputHash` | Changes once (the names inside it change). | Golden records both hashes. A test asserts every rule's hash changes in P1 and is stable in every later slice. |
| Eval cassette key (`hashScoreRequest` over `req.Features`) | Changes once. P1 re-records every committed cassette (fake and local only, per A1) and stamps `feature_key_space: ns-v1` in the cassette header. | Loading a cassette with a mismatched `feature_key_space` fails with a clear error rather than silently missing. |
| `run.json`: `dataset_sha`, `rule_sha`, `weights_sha` | `rule_sha` and `weights_sha` change once. `dataset_sha` changes only for corpus-v1 snapshot files, which P1 rewrites to v2. The manifest gains `feature_key_space`. | Golden compares metrics, not SHAs. A test checks `feature_key_space: ns-v1` is in the manifest. |
| `worker.renderReason` and stored verdict reasons | Prints namespaced names. Verdict rows gain `reason_version: 2`. Old stored reasons are history and are not rewritten. | A reason snapshot test, plus a test that the verdict row carries `reason_version`. |
| `corpus_examples.features` | New column `feature_key_space text NOT NULL DEFAULT 'flat-v0'`. P1's migration rewrites the JSON keys of existing rows (none in production, per A1; this touches local dev databases only) and sets `ns-v1`. Once migrated, the corpus loader refuses `flat-v0`. | Migration test on a seeded DB: keys rewritten and marker set. A second test checks a `flat-v0` row is refused. |
| `local.Version()`, which hashes the whole `Weights` struct | Changes once. The weights file's `version:` is bumped to `v2`. | `Version()` differs from the pre-rename golden value and is stable afterwards. |
| `feature.Names`, `Features.Map`, `config.FeatureSet`, `config/rules.yaml`, `config/local_weights.yaml`, `mutation_test.go`, `ablation_test.go`, golden-sign tables, `eval/fixtures/README.md` | Mechanical rename. | The full existing suite passes with renamed literals, and the grep test. |
| `abusekit score --jsonl` input rows (an external contract) | A row keyed by an old flat name is rejected with `feature_renamed`, and the error names the new name. It fails loudly and never translates silently. | CLI contract test. |

**The floating-point bound.** The local scorer sums weight × feature in sorted feature-name order.
Renaming the features changes the sort order, and with it the order of the additions. For `n`
terms, recursive summation error is at most `(n−1)·u·Σ|wᵢxᵢ|` with `u = 2⁻⁵³`. For e2a's rule:
- `n = 25` and `Σ|wᵢxᵢ| < 100` on any bounded vector, so `|Δlinear| < 2.7e-13`;
- the sigmoid's slope is at most ¼, so `|Δrisk| < 6.7e-14`.

The golden asserts `|Δrisk| ≤ 1e-12`, which leaves margin. So that tiers are provably unchanged, it
also asserts that no recorded score lies within `1e-12` of a tier cut point or a rule threshold. If
one ever does, the fixture is flagged rather than passing silently.

### 5.3 Name grammar and reserved namespaces

- **Features** match `^[a-z][a-z0-9]*\.[a-z][a-z0-9_]{0,55}$`. The namespaces `core`, `email`,
  `brand` and `custom` are reserved, as is any future pack name. Tenants define only `custom.*`,
  which is scoped to the tenant.
- **Event types** keep `^[a-z_.]+$`, are at most 64 bytes, and must contain a dot. The prefixes
  `subject.`, `payment.`, `subscription.`, `resource.`, `content.` and `abusekit.` are reserved.
- **Extension fields** on built-in types must be named `x_<name>`, so they can never collide with
  a future built-in field.

### 5.4 Product-declared vocabulary

There is no new built-in delivery type. The wire contract (`POST /v1/events`) keeps its shape;
every change below is additive and optional.

```yaml
vocabulary:
  version: 4                          # must increase with any change; history in §5.6
  subject_kinds:                      # default [account]
    account:  {}
    api_key:  {parent: account}       # a key's events also mark its parent account dirty
    card:     {}
  resource_kinds:
    key: {role: credential, aliases: [keys, api_key, api_keys, api-key, apikey, "api key"]}
    agent: {role: other}
  link_kinds:                         # in addition to the six built-in kinds
    phone_hash:   {evidence: true}    # counts as neighbour evidence
    oauth_sub_hash: {evidence: true}
  types:
    invite.sent:
      role: activity                  # included by core velocity/burst features
      fields:
        invitee_hash:   {kind: hash, join_domain: member}
        target_class:   {kind: enum, values: [own_community, other_community]}
        preview:        {kind: text, role: title, max_len: 200}   # stored as skeleton by default
        link_host:      {kind: domain}
  extend:                             # extension fields on built-in types
    resource.created:
      x_visibility: {kind: enum, values: [public, private]}
```

**Roles.** Only these ship:
- resource kinds: `credential` and `other` (the review's minimal set; an undeclared kind is
  `other`);
- types: `activity`;
- fields: `title`, which `brand.title_match` reads, and `self`, a bool marking a self-directed
  event that such features exclude.

`brand.title_match` counts distinct brands across the `title` fields of non-self activity events in
a trailing 1 h, capped at 3. A `title` field stored skeleton-only is matched through
`BrandSet.MatchesSkeleton`, which compares already-folded tokens to the brands' skeletons and
skips the second fold. (#7 found that double folding is not idempotent for leetspeak.)

**Subject kinds.** Events gain two optional fields:
- `subject_kind` (default `account`);
- `also: [{kind, id}]`, with at most 3 entries.

`also` lets one event (a charge attempt, say) be indexed under the merchant account, the card and
the customer at once:
- the event is stored once, and idempotency is still keyed on `(tenant, producer, id)`;
- one index row is written per subject;
- each named subject, and the declared `parent` of the primary subject, is marked dirty.

Subjects are keyed `(tenant, kind, id)`, and the API follows:
- `GET /v1/subjects/{subject}` and `POST .../evaluate` take an optional `?kind=` (default
  `account`);
- the list endpoint (S3b) gains a `kind` filter;
- rules declare `applies_to: [kinds]` (default `[account]`);
- a feature is computed only for the kinds in its `SubjectKinds`. For example, the `core`
  onboarding features exist only for `account`.

**Link kinds.** The `links` object gains `custom: {"<declared kind>": "<hash>"}`, with at most 8
entries. Each value is re-HMACed like every hash (§5.6). Built-in and declared evidence kinds feed
both `Neighbors` and the new `neighbours` op (§5.5).

**Legacy behaviour by declaration.** A profile with no `resource_kinds` gets the implicit
declaration `key: credential`, with #7's aliases. The golden replay proves this reproduces
`resourceKindAliases`.

### 5.5 Declarative custom features

#### Schema

```yaml
features:
  - name: custom.public_links_1h        # custom.* only
    version: 1                          # bump on any change (checked against history, §5.6)
    description: public share links created in the last hour
    subject_kinds: [account]            # default [account]
    count:                              # exactly one op key
      type: share.link_created
      where: {field: visibility, eq: public}
      sum: {field: size_class_weight, default: 1, cap_each: 10}   # optional
    window: 1h                          # or first: <dur> | lifetime | before_first: {type, where}
    transform: {log1p: true, cap: 50}   # cap mandatory
    prior_sign: "+"                     # optional, for uniform priors
```

**Windows.**

| Window | Range |
| --- | --- |
| `window: <dur>` | `(now − dur, now]`, where `dur` is 1 minute to 30 days, in whole minutes, hours or days |
| `first: <dur>` | `[start, start + dur)` |
| `lifetime` | `(now − 90d, now]`: the retention horizon. The docs call this "retained lifetime" so nobody reads it as all-time. |
| `before_first: {type, where}` | `(now − 90d, t_B]`, where `t_B` is the first matching event at or before `now`. If there is no such event, it falls back to `(now − 90d, now]`. The end is inclusive, matching #7's "at or before". |

Events with `at > now` are excluded from every window. See "Future events" under Semantics.

**Predicates.** A closed set, with no regex:
- equality and membership: `eq`, `ne`, `in` / `not_in` (at most 256 values; enum values are checked
  against the declaration);
- set files: `in_set` / `suffix_in_set` (named files of at most 100,000 entries, hashed at load);
- numeric comparison: `gt`, `gte`, `lt`, `lte`;
- presence: `exists`;
- combinators: `all`, `any`, `not`, with nesting depth at most 2 and at most 8 leaves.

A leaf is false when its field is absent or has the wrong type. `text` fields are banned from
predicates, `distinct`, `group_by` and `on`.

**Operations.** `cap(v)` below is the transform.

| Op | Definition at `now` | Bound / cost class |
| --- | --- | --- |
| `count` | The number of matching events in the window, or the sum of `sum.field` over them. An absent field counts as `default`, and each value is clamped to `cap_each`. | O(n) |
| `distinct` | `{field}`: the number of distinct values of `field` among matches. Tracking stops at `track_max`, the smallest count whose transformed value reaches `cap`; `track_max` must be ≤ 10,000. | O(n); memory O(track_max) |
| `share` | `{where, match, sum?}`: the numerator over the `match` events divided by the denominator over the `where` events. When the denominator is 0, the value is `if_empty` (default 0). | O(n) |
| `peak` | `{size}`: the maximum of `agg(E ∩ (t − size, t])`, taken over `t` = the instants of matching events inside the outer window. Sub-windows are **clipped** to the outer window, so events outside it never count even if they fall inside `(t − size, t]`. Requires `size` ≤ window and window ÷ size ≤ 1440. | O(n), two-pointer |
| `time_between` | `{from: {type, where, anchor: first\|last}, to: {type, where}, until_now, if_absent}`. `t_A` is the first (or last) matching `from`; `t_B` is the first matching `to` with `t_B ≥ t_A`. The value is minutes from `t_A` to `t_B`. With `until_now` and no `t_B`, it is `now − t_A`. `if_absent` is mandatory. Floored at 0. | O(n) |
| `sequence` | `{a: {type, where}, b: {type, where}, within: <dur>, on: {a: field, b: field}?}`: the number of `b` events in the window that have at least one `a` event with `t_a ∈ (t_b − within, t_b]` and, if `on` is set, `a.on == b.on`. Both `on` fields must be `hash` fields with the **same `join_domain`** (§5.6), or equality across them is meaningless; the loader checks this. The evaluator keeps, per `on` value, the latest `a` instant ≤ `t_b` in an LRU capped at 10,000 keys. Eviction sets the feature's truncated flag. Requires `within` ≤ 24 h. | O(n); memory O(keys) |
| `group_by` | A modifier on `count`, `distinct` or `sum`: `{field, reduce: max\|{count_gte: k}, max_groups ≤ 1000}`. Matches are grouped by `field` and the op is applied per group. `max` returns the largest group value; `count_gte: k` returns how many groups have a value ≥ k. Groups are created in `(at, producer, id)` order. Once `max_groups` is reached, new groups are ignored and the truncated flag is set. | O(n); memory O(groups) |
| `ratio` | `{num: custom.a, den: custom.b, if_empty}`: `num ÷ den` over two **non-ratio** custom features, forming a depth-1 DAG. The loader rejects cycles and ratio-of-ratio. | O(1): the inputs are computed anyway |
| `neighbours` | `{via: [link kinds], where: {deleted: permanent} \| {labelled: abusive} \| {created_within: <dur>} \| {}}`: the number of distinct other same-tenant, same-kind subjects that share any `via` key and satisfy the condition. Fan-in is capped at 50 per key and 200 in total, as in main §4.2, and hitting a cap sets `core.neighbors_truncated`. | One store query per distinct `via` set, cached per extraction. At most 4 `neighbours` features per tenant. |

**`relative_to_history`.** A modifier on `count`, `distinct` and `peak`. It is the exact pipeline
of #7's `burstFactor × ageDecayFactor`:

```
cur      = op over the feature's window at now
E_b      = { matching events e : now − lookback < e.at ≤ now − exclude_recent }
base     = baseline_op over E_b      (default: the same op; for count/distinct over window W it is
           the max of that op over sliding windows (t − W, t], t ∈ instants of E_b, clipped to E_b;
           for peak it is peak over E_b with the same size)
v1       = min( cur / max(base, 1), ratio_cap )          ratio_cap defaults to transform.cap
age_days = (now − start) / 24h
d        = clamp( 1 − (age_days − full_until)/(zero_at − full_until), floor, 1 )   if age_decay
v2       = v1 · d                                         (v1 if no age_decay)
value    = transform(v2)                                  log1p (optional), then cap
```

Parameters:
- `lookback` ≤ 30 d, `exclude_recent` < `lookback`, and `floor` > 0.
- `age_decay` defaults to `full_until: 3d, zero_at: 30d, floor: 0.2`, which are #7's constants.
- `baseline:` may override the baseline op, e.g. `baseline: {peak: {size: 10m}}`. That lets a 1 h
  sum be compared against a prior 10-minute peak, which is how #7's `sends_1h` works.

**Which #7 and S2 features the DSL can express:**
- **Expressible:**
  - `sends_10m_max`, `sends_1h`, `webmail_sends_1h`: `relative_to_history` with a `baseline`
    override, `sum: recipient_count`, `cap_each: 300`, `ratio_cap: 300`;
  - `sends_first_day`;
  - `webmail_recipient_share`: `share` + `in_set: webmail`;
  - `declines_before_first_success`, and `self_send_before_external` with `cap: 2`: via
    `before_first`;
  - `resource_*`, `credential_*`;
  - `upgrade_delay_min`: `time_between` + `cap`.
- **Not expressible:**
  - `distinct_recipients_1h`, a mixed aggregate: distinct hashes, falling back to summing
    `recipient_count` for events that have no hash;
  - `subject_brand_match` and `brand.*`, which need the brand matcher and its integration and
    community gates;
  - `first_day_distinct_domains`, which has an inclusive end where DSL `first:` windows are
    half-open;
  - `burst_ratio_24h_vs_lifetime`, which counts future-dated events in its lifetime denominator;
  - `linked_*` and `fingerprint_*`, which carry specific deleted-and-labelled evidence semantics;
    the `neighbours` op covers the generic cases;
  - `subject_age_h`, which reads `start` rather than events.

The non-expressible features stay Go features in their packs. P4b adds a test that re-expresses
every "expressible" feature in the DSL and checks it matches the Go value bit for bit on every
fixture.

**Transform.** `cap` is mandatory: a finite value > 0, and at most 1 for `share`. `log1p` is
optional and is applied before the cap:
- `log1p: true` gives `ln(1+v)`;
- `log1p: {scale: s}` gives `s·ln(1+v)`;
- `log1p: {anchored_at: n}` sets `s = n/ln(1+n)`.

The output always lies in `[0, cap]`, and that range is the feature's `Bound`.

#### Semantics

- **Pure and order-independent.** A feature is a function of the loaded event set and `now`.
  Ordering, ties and "first" all use `(at, producer, id)`. P0 switches the store's scoring loader
  from `(at, seq)` to this order, and the golden captures it before the rename. Any fixture whose
  ties now resolve differently is listed as a baseline change: a tie resolved by arrival order was
  a latent non-determinism.
- **Half-open windows.** `(now − W, now]`, `[start, start + W)`, and clipped `peak` sub-windows.
  `before_first` is the one intentionally inclusive end.
- **Future events.** Events with `at > now` are excluded from every custom window, and schedule a
  rescore at their `at`. Some Go features keep frozen legacy behaviour for semantic identity,
  documented on each `FeatureDef`:
  - `core.resource_total`, `core.credential_total` and `core.burst_ratio_24h_vs_lifetime` count
    future-dated events;
  - `email.first_day_distinct_domains` has an inclusive end.

  Harmonising them is `core@2`/`email@2` work (§12 Q5).
- **Deterministic floats.** Sums accumulate in `(at, producer, id)` order in one `float64`.
- **Rescore candidates.** Each DSL feature emits:
  - the exit time of its oldest in-window match;
  - the end of any anchored window;
  - for `peak`, the exit of the current maximum's sub-window;
  - for `relative_to_history`, the crossings of `now − exclude_recent` and `now − lookback`;
  - for `before_first`, the first `to` event;
  - future-dated matches.

  Rescore-storm control (§5.8) then filters and coalesces them.

### 5.6 Redaction, pseudonymisation and vocabulary history

Ingest never consults rules. It consults the tenant's **vocabulary**. What abusekit stores is
**pseudonymised**, not anonymous: anyone holding both the key and a candidate value can recompute
a keyed hash. Retention and erasure (main §4.4, §4.11) therefore apply to it.

**Field kinds:**

| Kind | Stored as | Rejected when |
| --- | --- | --- |
| `text` | NFKC. Email, card, IP and phone shapes are **masked** (`@`, `#card`, `#ip`, `#phone`), and the value is truncated at `max_len` (≤ 500). **`store: skeleton` is the default for custom text**: only the confusables skeleton is kept. `store: raw` is opt-in, for text-accepting scorers only. The built-in `content.sent.subject_line` stays raw (main §4.3). | Control characters, invalid UTF-8 |
| `number` | A finite float64, checked against the optional `min`, `max` and `integer`. Integers of 13–19 digits also get the Luhn check. | Out of range, non-finite, or Luhn-valid |
| `bool` | As-is | Not a bool |
| `enum` | One of the declared values (at most 64, each matching `[a-z0-9_.-]{1,64}`) | Undeclared value |
| `hash` | **Re-HMACed at ingest:** `hk<keyid>:` + hex(HMAC-SHA256(k_tenant, input))[:32]. `input` is `lp(type) ‖ lp(field) ‖ lp(value)`, or `lp("join:" ‖ join_domain) ‖ lp(value)` when the field declares a `join_domain`; `lp` is a u32 length prefix. A `join_domain` makes the same identifier equal across fields and types (for `sequence.on` and cross-type `distinct`); without one, hashes are separated per field. The producer's value is never stored. | Doesn't match `^[A-Za-z0-9_:+/=-]{8,128}$` |
| `domain` | Lower-cased and IDNA-encoded to ASCII. It must end in a public suffix from the embedded, versioned PSL snapshot, or in an RFC 6761 special-use name (`.test`, `.example`, `.invalid`, `.localhost`) so synthetic fixtures stay valid. With the optional `reduce: etld1`, only the registrable domain is stored. | IP literal, all-numeric label, unknown suffix, `@` |
| `timestamp` | RFC 3339 (#7's pattern) | Anything else |

**Rules, in order:**
1. **Leak scan.** It runs over every key and value of every event, and now detects:
   - email addresses;
   - Luhn-valid runs of 13–19 digits (separators allowed);
   - IPv4 and IPv6 literals;
   - phone shapes: `+` followed by 8–15 digits, or grouped national formats of 10 or more digits.

   What happens on a match depends on the field:
   - a declared `text` field is masked;
   - a declared `hash` field is exempt, because its value is replaced by the HMAC;
   - anywhere else, the event is rejected with `redaction_failed`.
2. **Declared fields** are handled by kind, as in the table above.
3. **Undeclared fields** of any type, built-in or declared, are **dropped whatever their kind**,
   numbers included. The row keeps `x_undeclared: [sorted field names]`; a name that fails the
   leak scan is rejected. **Undeclared types** keep only `type`, `at` and that list of names.
   Revision 1 hashed undeclared strings instead; that is removed.
4. **Built-in hash values are re-HMACed too:** `content.sent.recipient_hash` and every `links`
   value, both the six built-in link kinds and declared ones. Every `links` kind has its own
   `join_domain`, which is the kind's name. Equality is preserved, so `distinct` counts,
   neighbour joins and every golden feature value are unchanged; only the stored bytes change. A
   producer that mistakenly sends a raw phone number as a "hash" never has it stored.
5. **Built-in domain fields** (`recipient_domain`, `address_domain`, `first_link_host`) get the
   `domain` rules, and `RedactionSchemaVersion` becomes 3. Every committed fixture uses `.test`
   and passes.

**Keys and rotation.** The key interface is provider-agnostic:

```go
type Keys interface {
    // Current returns the key used to write new values, and its id.
    Current(ctx context.Context, tenant string, purpose Purpose) (id string, key []byte, err error)
    // ReadSet returns every key readers must accept right now (current, plus the previous key
    // during a rotation).
    ReadSet(ctx context.Context, tenant string, purpose Purpose) ([]KeyRef, error)
}
```

There are two adapters: a file adapter for dev and tests, and a cloud secret-manager adapter.
Neither the names nor the config mention a provider. Rotation is **dual-key**:
- For `max_lookback` (at most 30 days), ingest writes each hash field twice: `<field>` under the
  new key and `<field>__prev` under the old one. Links get one row per key id.
- Until the rotation's `read_flip_at`, features read the `__prev` values and neighbour joins match
  either key id.
- After `read_flip_at`, readers use only the new values and `__prev` writes stop.
- The key id is part of `vocab_version` (`"<tenant>@<n>/k<id>"`), so every row can be traced to
  the key that hashed it.

**Vocabulary history lives in the config tree.** The mount holds `tenants/<t>/history/NNNN.yaml`,
an append-only list of accepted vocabulary and custom-feature versions, each with an
`effective_at`. CI in the private config repo runs `abusekit config check` over the full history
and enforces three rules:
- **Widening only.** Adding a type, field, enum value, `max_len` or `max` is fine. Changing a
  field's kind, removing an enum value or narrowing a limit needs a new field name.
- **No redefinition.** A custom feature's `(name, version)` is never redefined.
- **Monotonic time.** `effective_at` never goes backwards.

At runtime, the `tenant_config_versions` table records what was actually loaded and refuses a
profile that contradicts it. It is a guard, never the source of truth.

**Warm-up.** A feature is **cold** from its `effective_at` until
`effective_at + max(window, lookback + exclude_recent)`. That applies to a new custom feature, a
new version of one, and any feature over a newly declared field or type. An `advise` rule with a
cold input is scored and stored as shadow for that period: it reports `mode: shadow` with
`warming_until`. The value is still computed; it just can't drive a tier on partial history.

### 5.7 Bounded loading, step budget, and truncation as a signal

This replaces revision 1's bound of the 50,000 newest events and its 250 ms wall-clock deadline.

**Load plan (per profile and subject kind, computed at compile time):**
1. **Onboarding types, in full.** `subject.*`, `payment.*` and `subscription.*` are loaded
   oldest-first, up to `onboarding_bytes` (1 MiB decoded by default). Onboarding facts such as
   "first success" come from the *earliest* events, so recent activity can never push them out.
2. **Anchored range.** `[start, start + A)` is loaded oldest-first, where `A` is the maximum
   `first:` duration (24 h for e2a).
3. **Trailing range.** `(now − L, now + 24h]` is loaded **newest-first** until the total decoded
   size reaches `history_bytes` (8 MiB by default).
   - `L = max over features of max(window, lookback + exclude_recent + baseline width)`.
   - `L` is 90 d if any feature uses `lifetime` or `before_first`.
   - The `+24h` covers future-dated events inside the skew allowance, which schedule rescores.
4. **`start`** comes from `subject.created.account_created_at` if present, else from
   `subjects.first_seen_at`, never from loaded events. Truncation therefore can't move it.

Events are deduplicated across the three ranges by `(producer, id)`, and the combined history is
ordered `(at, producer, id)`.

**Step budget, instead of wall-clock time.** A step is one event dispatched to one feature, plus
one step per predicate leaf. The per-subject budget is
`steps_max = history_bytes / avg_event_bytes × per_type_fanout_max × leaves_max`. With the defaults
that is 8 MiB / 256 B × 16 × 8 ≈ 4.2M. Every pack charges its steps through `Input.Budget`, the Go
packs included.

The P4a benchmark calibrates the ns-per-step figure end to end (store load, decode, projection,
extraction, orchestration). The committed `cost_table.yaml` records it, and CI fails if a profile
at the limits exceeds p99 50 ms. Because the budget counts work, when it runs out doesn't depend
on how fast the host is.

**Exhaustion and truncation.** Exhausting the byte cap, the step budget, `max_groups` or the
`sequence` keys never makes a rule unscored:
- features are computed over what was processed, in a deterministic order (newest-first for
  trailing features, earliest-first for onboarding and anchored ones);
- `core.history_truncated = 1` is set, and `core` requires it to carry a **positive** weight.

A pack **error** (a bug) still marks the rules that read that pack as unscored and degraded.
Truncation never does.

**Why flooding with cheap events can't evade:**
- (a) Onboarding facts and `start` are loaded separately and are never displaced.
- (b) Newest-first loading keeps every trailing window, up to the byte cap. The flood is itself the
  most recent activity, so it is counted, and it raises every volume or velocity feature it
  matches.
- (c) Truncation drops only the *oldest* trailing events, which feed three things:
  - baselines: a smaller baseline gives a larger `v1`, because `cur / max(base, 1)` is monotone;
  - lifetime denominators: a smaller denominator gives a larger ratio (as with `burst_ratio`);
  - lifetime totals: a smaller total gives a smaller value. This is the only direction that can
    lower risk.
- (d) An invariant closes the third case. `packtest` checks it for every weights file that
  includes `core.history_truncated`:
  `w(core.history_truncated) ≥ Σ over features f with TruncationDir(f) = −1 of |w_f| · Bound_f`.
  Truncation therefore never lowers the linear sum. Two Go features have no natural bound
  (`core.resource_total`, `core.credential_total`). They get `Bound` from a cap of 1,000, which no
  fixture reaches, so semantic identity holds.
- (e) Forcing truncation sets the abusive subject's own `history_truncated` signal.

Criterion 6's property test covers every built-in and DSL feature.

**Limits** (validated at load; also the fuzz oracle):

| Limit | Value |
| --- | --- |
| Custom features per tenant | 64 |
| Features per event type (fan-out) | 16 |
| Predicate depth / leaves | 2 / 8 |
| `in` list size / set-file entries | 256 / 100,000 |
| Window / lookback | ≤ 30 d (`lifetime` and `before_first` are 90 d: retention) |
| `peak` window ÷ size | ≤ 1440 |
| `distinct` track_max / `group_by` groups / `sequence` keys | 10,000 / 1,000 / 10,000 |
| `neighbours` features | 4 |
| `also` subjects per event / declared link kinds | 3 / 8 |
| `history_bytes` / `onboarding_bytes` | 8 MiB / 1 MiB decoded |
| Steps per subject | Calibrated; default ≈ 4.2M |
| Static cost units per tenant | 256. A unit is the benchmarked cost of one O(n) op at the byte cap. |

### 5.8 Profiles, rules, scheduling

**Profiles live in a private mount** (`--profiles /etc/abusekit/tenants/`, mounted by the hosted
deploy from the operator's private config repo). This repo ships only:
- `examples/tenants/*.yaml`: the five fictional §7 profiles;
- `examples/tenants/reference/`: today's `config/rules.yaml` + `local_weights.yaml`, renamed. The
  golden replay runs this profile, and e2a's private profile starts as a copy of it.

**Validation.** It adds these codes to main §4.5's list. Any failure rejects the whole profile,
with every error collected:
- `pack_unknown`, `pack_requires`
- `feature_unknown`, `feature_not_enabled`, `feature_namespace`, `duplicate_feature`,
  `feature_version_reused`
- `dsl_invalid` (with a JSON-pointer path)
- `vocab_invalid`, `vocab_incompatible`
- `weights_unknown_feature`, `uniform_not_shadow`, `truncation_weight_insufficient`
- `subject_kind_unknown`

**Per-tenant everything:**
- **Reload.** Atomic per tenant. A rejected profile keeps that tenant's previous profile live and
  never affects another tenant. `/healthz` reports `config_error{tenant}`.
- **Rule sets.** `worker.computeVerdict` and `serve.currentRuleNames()` both read the subject's
  tenant profile, not a global config. P2's test: tenant A's retired rule never appears in tenant
  B's view.
- **Scorer version.** Each tenant's local scorer is bound to its own weights file. Its
  content-derived version (`local@<sha256[:12]>`) appears in the verdict `model` field and in the
  signals of `GET /v1/subjects/{id}`.

**Stage gates consider only advise-mode local rules.** `maxRiskByScorer` excludes shadow rules, so
a shadow experiment can never open or close a vendor call's gate. This changes behaviour on
`main`, but e2a has no staged rules, so the golden replay is unaffected.

**Scheduling:**
- **Per-tenant fair queue.** The worker claims dirty subjects round-robin across tenants, with
  per-tenant weights (equal by default) and a per-tenant concurrency cap (4 of the batch by
  default). One tenant's backlog can't starve another. Main §4.8's priority order applies within a
  tenant.
- **Rescore-storm control.** Timer rescores (as opposed to event-driven dirty marks) have three
  limits:
  - they are scheduled only from features that feed at least one non-shadow rule;
  - DSL features coalesce into buckets of `max(5 min, W / 12)`, while built-in Go features keep
    the fixed 5-minute bucket for semantic identity;
  - a per-tenant timer-rescore budget (default 20 × active subjects per hour) is enforced by the
    queue. When it is exhausted, timer rescores defer to the next hour and a metric counts them.

  Event-driven scoring is never budgeted.

### 5.9 Weights, eval and bootstrap per pack

- **Weights.** Per rule, keyed by namespaced name. The local math is unchanged.
- **Starter weights.** Each pack has `config/packs/<pack>/starter.yaml`, with one rule named
  `<pack>_starter`. Every weight has a `sign:`. `core`'s starter includes
  `core.history_truncated`, set so the §5.7 invariant holds.
- **Uniform priors, shadow only.** `weights: uniform` binds a local scorer with, for `k` inputs:
  - normalisation `x̂ᵢ = min(max(xᵢ, 0), Boundᵢ) / Boundᵢ`, which clamps to `[0, 1]`;
  - `wᵢ = sᵢ · 4/k`, where `sᵢ` is the input's prior sign: `FeatureDef.PriorSign` for pack
    features, `prior_sign` for custom ones, default `+`;
  - `bias = −2 + (4/k) · |{i : sᵢ = −1}|`.

  So `risk ∈ [sigmoid(−2), sigmoid(2)] ≈ [0.12, 0.88]`. The loader rejects uniform weights in
  advise (`uniform_not_shadow`). Promotion needs a real weights file plus a gate run.
- **Eval by profile.** `abusekit eval --profile examples/tenants/<x>` or `--pack <p>`.
  - Floor entries gain `profile:`; an entry without one belongs to the reference profile.
  - The manifest gains `profile_sha`, `pack_versions` and `feature_key_space`.
  - Corpus v2 is corpus-v1 plus `profile`, `vocab_version` and `feature_key_space: ns-v1`.
- **`packtest`** runs for every pack in CI and checks:
  - starter-weight signs match their `sign:`;
  - zeroing any weight moves a pack fixture band or an isolated scenario;
  - results are deterministic under shuffled arrival;
  - no future leakage;
  - the pack stays in its namespace and within `Bound`;
  - the truncation invariant holds;
  - the flood property (criterion 6).
- **Held-out fixtures** for the bootstrap criterion live in
  `examples/tenants/<x>/fixtures/{dev,heldout}/`.
  - Held-out fixtures are authored in a separate commit after the features are frozen, with
    non-overlapping generator seeds.
  - CI evaluates criterion 2 only on the held-out set.
  - A PR that changes a scenario's features and its held-out fixtures together fails a CI check;
    held-out fixtures change only in their own PR.
- **Bootstrap.** A new product:
  1. writes its profile;
  2. runs `core_starter` (plus `brand_starter` if it has display names) and a `custom_uniform`
     rule, all in shadow;
  3. collects labels through `POST /v1/labels`;
  4. once the harness passes on its labelled set, hand-tunes a weights file and promotes it
     through the normal path.

### 5.10 Storage (expand-only)

- `events`: add `vocab_version text NULL` and `subject_kind text NOT NULL DEFAULT 'account'`.
- New table `event_subjects(tenant, subject_kind, subject, event_seq)`. It indexes `also` and
  parent rows, and the scoring loader reads through it.
- `subjects`: the key becomes `(tenant, kind, subject)`, via a new `kind` column (default
  `account`) and a unique index.
- `links`: add `key_id` for rotation.
- `corpus_examples`: add `feature_key_space`.
- `verdicts`: add `profile_sha` and `reason_version`.
- New table `tenant_config_versions`, the runtime guard (§5.6).

**S3b erasure must be vocabulary-aware.** Which stored fields are raw text, skeleton or
pseudonymised hash depends on each row's `vocab_version`, and the erasure ledger records the
version it applied. S3b's design pass must include this.

### 5.11 API surface summary

| Surface | Change | Compatibility |
| --- | --- | --- |
| `POST /v1/events` | Optional `subject_kind`, `also`, `links.custom` and `x_` extensions; declared types; stricter domain kind; card/IP/phone scan; re-HMAC; undeclared values dropped | Wire additive. Storage semantics change (pre-GA; decisions Q3, Q14). |
| `GET /v1/subjects/{id}`, `evaluate` | Optional `?kind=`; per-tenant `model` version; reasons use namespaced names; `warming_until` on warming signals | Additive |
| Per-item codes | None new | Unchanged |
| `abusekit score --jsonl` | Namespaced names; flat names return `feature_renamed` | Breaks once, pre-GA (P1) |
| Config | Per-tenant profiles in a private mount; `rules.yaml` replaced by `examples/tenants/reference` | Breaks once, pre-GA |

**Rejected: runtime vocabulary declaration over HTTP** (`PUT /v1/vocabulary`). It would let a
producer key widen its own redaction boundary.

### 5.12 Alternatives considered

- **CEL.** It has no windowed aggregates, so we would still write every op. It is also a large
  dependency, costs expressions rather than histories, and gives product engineers worse errors.
  It remains a possible later leaf kind for `where` only (§12 Q9).
- **A home-grown expression language.** A parser, a grammar and an injection surface, with no
  coverage beyond the closed ops.
- **A plugin ABI.** Go `plugin` is fragile and unsandboxed; WASM is code disguised as config, and
  reviewers can't read it. Code belongs in a Go pack upstream, gated by `packtest`.
- **SQL features.** They would couple features to the store's schema, give unbounded cost, and
  put tenant isolation at risk.
- **A canonical-key bridge (revision 1).** It lost because nothing stored needs it (A1), and it
  would keep two names alive forever.
- **A neutral built-in `delivery.sent` (revision 1).** It lost because "delivery" is still an
  email-shaped abstraction with channels bolted on. Declared types with field roles are genuinely
  neutral, and `content.sent` stays the email pack's own type.
- **Hashing undeclared strings (revision 1).** It lost because the hash of an unreviewed field is
  still pseudonymous personal data nobody asked for. Dropping it is strictly safer, and declaring
  a field is cheap.
- **Wall-clock deadlines (revision 1).** They lost because they are non-deterministic and
  host-dependent, and because a timeout that marks a rule unscored rewards flooding. A step
  budget plus truncation-as-signal has neither problem.

## 6. Edge cases and failure handling

- **A pack errors (a bug).** Rules that read that pack are unscored with `feature_error` and
  marked degraded; other rules still score. Hitting a budget or truncating is not an error (§5.7).
- **A profile is rejected.** The tenant's previous profile stays live. On a cold start with no
  valid profile, the tenant's scores are `unknown` and `/healthz` is red. Another tenant's rules
  are never borrowed.
- **Events arrive before their declaration.** Undeclared data is dropped (§5.6 rule 3). A feature
  declared later sees only the type and time of those rows. Warm-up keeps rules that use it in
  shadow until its window is fully covered.
- **Unknown values.**
  - An undeclared resource kind is treated as `other` and counted in a metric; in `strict` mode it
    is rejected.
  - An undeclared `subject_kind` or `also` kind is rejected with `redaction_failed`.
  - An undeclared link kind is rejected with `bad_links`.
- **Absent fields.** A predicate on an absent field is false, and `sum` uses `default`.
  `share`/`ratio` use `if_empty`, and `time_between` uses `if_absent`. Every op is total; a
  non-finite value is a pack error.
- **Duplicates, out-of-order arrival, ties.** Idempotency is unchanged. Features are set functions
  over the history ordered `(at, producer, id)`.
- **Clock skew and future events.** They are excluded from custom windows, but loaded (within
  24 h) so they can schedule a rescore. The legacy exceptions are listed in §5.5.
- **Key rotation during a burst.** Dual keys (§5.6) keep `distinct` and neighbour equality exact.
- **`also` abuse.** A producer naming arbitrary subjects is limited to 3 per event. Each dirty
  mark counts against the per-tenant rescore and scoring budgets.
- **Parent fan-in.** Many `api_key` subjects mark the same parent account dirty. Dirty marks
  coalesce per subject through `dirty_seq`, so the parent costs O(1) per scoring round.
- **Hostile config.** No code or regex, every set hashed, every size capped, and the loader is
  fuzzed.
- **Hostile events.** Byte caps, the step budget, and capped groups and keys bound the cost.
  Truncation raises risk rather than lowering it (§5.7).
- **The brand pack on a marketplace that resells branded goods.** `brand.*` stays in shadow until
  the tenant's own labels justify it. The tenant may also narrow the brand list (§12 Q10).

## 7. Genericity walk: five scenarios (fictional)

All products, ids and domains are invented; timestamps use the 2031 convention. Each profile is
committed in P6b as `examples/tenants/<name>/`, with dev and held-out fixtures.

### 7a. File sharing: malware-distribution burst ("Driftbox")

The pattern: a fresh account uploads executables or archives and creates many public links, which
are then downloaded from many distinct networks within the hour.

```yaml
packs: [core@1, brand@1]
vocabulary:
  version: 1
  resource_kinds: {api_token: {role: credential}}
  types:
    share.link_created:
      role: activity
      fields:
        visibility: {kind: enum, values: [public, org, private]}
        file_kind:  {kind: enum, values: [document, archive, executable, image, other]}
        folder_title: {kind: text, role: title, max_len: 120}      # skeleton-only
    share.downloaded:
      fields:
        link_hash:           {kind: hash}
        downloader_ip24:     {kind: hash}
        downloader_is_owner: {kind: bool, role: self}
features:
  - {name: custom.public_links_1h, version: 1, description: public links in the last hour,
     count: {type: share.link_created, where: {field: visibility, eq: public}},
     window: 1h, transform: {log1p: true, cap: 6}}
  - {name: custom.risky_link_share_24h, version: 1, description: links to executables or archives,
     share: {type: share.link_created, match: {field: file_kind, in: [executable, archive]}},
     window: 24h, transform: {cap: 1}}
  - {name: custom.distinct_downloader_nets_1h, version: 1, description: distinct downloader networks,
     distinct: {type: share.downloaded, where: {field: downloader_is_owner, eq: false}, field: downloader_ip24},
     window: 1h, transform: {log1p: true, cap: 9}}                 # track_max 8103
  - {name: custom.max_downloads_per_link_1h, version: 1, description: busiest link's downloads,
     count: {type: share.downloaded, where: {field: downloader_is_owner, eq: false},
             group_by: {field: link_hash, reduce: max, max_groups: 1000}},
     window: 1h, transform: {log1p: true, cap: 8}}
  - {name: custom.download_peak_vs_history, version: 1, description: 10-min download peak vs own past,
     peak: {type: share.downloaded, size: 10m}, window: 24h,
     relative_to_history: {lookback: 30d, exclude_recent: 24h, age_decay: {}},
     transform: {log1p: true, cap: 6}}
  - {name: custom.signup_to_first_public_link_min, version: 1, description: minutes to first public link,
     time_between: {from: {type: subject.created}, to: {type: share.link_created, where: {field: visibility, eq: public}},
                    until_now: true, if_absent: 1440},
     transform: {cap: 1440}, prior_sign: "-"}
rules:
  - {name: malware_burst, mode: shadow, scorer: local, weights: uniform,
     inputs: [custom.public_links_1h, custom.risky_link_share_24h, custom.distinct_downloader_nets_1h,
              custom.max_downloads_per_link_1h, custom.download_peak_vs_history,
              custom.signup_to_first_public_link_min, brand.title_match, core.history_truncated],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

```json
{"id":"db-003","subject":"acct_example_db_1","type":"share.link_created","at":"2031-03-02T09:03:00Z","data":{"visibility":"public","file_kind":"archive","folder_title":"Invoice Center"}}
{"id":"db-005","subject":"acct_example_db_1","type":"share.downloaded","at":"2031-03-02T09:05:02Z","data":{"link_hash":"lk_4f1c9a0e7b2d11","downloader_ip24":"ip_9a1b2c3d4e5f66","downloader_is_owner":false}}
```

**Declarative:** everything above. **Go-only:** file-content verdicts, such as a malware hash or a
sandbox result. The product emits these as `content.verdict`, and `core.verdict_max_24h` reads
them.

### 7b. Marketplace: card testing ("Tallyport")

The pattern: a merchant account pushes many small charge attempts across many cards, and most are
declined. A card that turns up across many merchants is suspicious in itself.

```yaml
packs: [core@1, brand@1]
vocabulary:
  version: 1
  subject_kinds: {account: {}, card: {}}
  resource_kinds: {api_key: {role: credential}}
  types:
    charge.attempted:
      role: activity
      fields:
        outcome:      {kind: enum, values: [succeeded, declined, blocked]}
        decline_code: {kind: enum, values: [insufficient_funds, do_not_honor, incorrect_cvc, expired_card, fraudulent, other]}
        amount_minor: {kind: number, min: 0, integer: true}
        card_hash:    {kind: hash, join_domain: card}
features:
  - {name: custom.max_declines_per_card_1h, version: 1, description: most declines on one card,
     count: {type: charge.attempted, where: {field: outcome, eq: declined},
             group_by: {field: card_hash, reduce: max, max_groups: 1000}},
     window: 1h, transform: {cap: 20}}
  - {name: custom.cards_with_3plus_declines_1h, version: 1, description: cards declined 3+ times,
     count: {type: charge.attempted, where: {field: outcome, eq: declined},
             group_by: {field: card_hash, reduce: {count_gte: 3}, max_groups: 1000}},
     window: 1h, transform: {log1p: true, cap: 6}}
  - {name: custom.distinct_cards_1h, version: 1, description: distinct cards,
     distinct: {type: charge.attempted, field: card_hash}, window: 1h, transform: {log1p: true, cap: 7}}
  - {name: custom.small_charges_1h, version: 1, description: charges at or under 200 minor units,
     count: {type: charge.attempted, where: {field: amount_minor, lte: 200}}, window: 1h, transform: {cap: 500}}
  - {name: custom.charges_1h, version: 1, description: all charges,
     count: {type: charge.attempted}, window: 1h, transform: {cap: 500}}
  - {name: custom.small_charge_ratio_1h, version: 1, description: small ÷ all charges,
     ratio: {num: custom.small_charges_1h, den: custom.charges_1h, if_empty: 0}, transform: {cap: 1}}
  - {name: custom.declines_10m_peak, version: 1, description: declines in busiest 10 min today,
     peak: {type: charge.attempted, where: {field: outcome, eq: declined}, size: 10m},
     window: 24h, transform: {log1p: true, cap: 7}}
  - {name: custom.card_merchants_24h, version: 1, subject_kinds: [card],
     description: distinct merchants that charged this card,
     distinct: {type: charge.attempted, field: x_primary_subject_hash}, window: 24h, transform: {cap: 50}}
rules:
  - {name: card_testing, mode: shadow, scorer: local, weights: uniform, applies_to: [account],
     inputs: [custom.max_declines_per_card_1h, custom.cards_with_3plus_declines_1h, custom.distinct_cards_1h,
              custom.small_charge_ratio_1h, custom.declines_10m_peak, core.credential_velocity_1h,
              core.history_truncated], labels: [benign, abusive], benign_label: benign, threshold: 0.7}
  - {name: tested_card, mode: shadow, scorer: local, weights: uniform, applies_to: [card],
     inputs: [custom.card_merchants_24h], labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

Each charge is also indexed under the card subject, through `also`:

```json
{"id":"tp-101","subject":"acct_example_tp_7","also":[{"kind":"card","id":"card_example_c1"}],"type":"charge.attempted","at":"2031-06-10T02:14:01Z","data":{"outcome":"declined","decline_code":"incorrect_cvc","amount_minor":100,"card_hash":"cd_1a2b3c4d5e6f77"}}
```

`x_primary_subject_hash` is a **derived** field. Ingest adds it to every row indexed through
`also`, as the re-HMACed id of the primary subject (join domain `subject:<kind>`). It is declared
implicitly for any type used with `also`, so a secondary subject can count distinct primaries.

**Declarative:** everything above. **Go-only:** issuer and BIN intelligence, and velocity seen by
external card networks. Products can supply these as `content.verdict` or enum fields.

### 7c. Chat or community: spam invites ("Hearthchat")

The pattern: new accounts with brand-like names invite people outside their own communities,
often with links, and the recipients block them soon after.

```yaml
packs: [core@1, brand@1]
vocabulary:
  version: 1
  link_kinds: {phone_hash: {evidence: true}}
  types:
    invite.sent:
      role: activity
      fields:
        invitee_hash: {kind: hash, join_domain: member}
        target_class: {kind: enum, values: [own_community, other_community]}
        preview:      {kind: text, role: title, max_len: 200}
        link_host:    {kind: domain, reduce: etld1}
    block.received:
      fields: {blocker_hash: {kind: hash, join_domain: member}}
features:
  - {name: custom.invites_10m_peak, version: 1, description: invites in busiest 10 min today,
     peak: {type: invite.sent, size: 10m}, window: 24h, transform: {log1p: true, cap: 7}}
  - {name: custom.external_invite_share_24h, version: 1, description: invites outside own communities,
     share: {type: invite.sent, match: {field: target_class, eq: other_community}}, window: 24h, transform: {cap: 1}}
  - {name: custom.invites_blocked_within_10m, version: 1, description: invitees who blocked within 10 min,
     sequence: {a: {type: invite.sent}, b: {type: block.received}, within: 10m, on: {a: invitee_hash, b: blocker_hash}},
     window: 24h, transform: {log1p: true, cap: 6}}
  - {name: custom.linked_invite_share_24h, version: 1, description: invites with links,
     share: {type: invite.sent, match: {field: link_host, exists: true}}, window: 24h, transform: {cap: 1}}
  - {name: custom.phone_siblings_7d, version: 1, description: accounts sharing a phone created this week,
     neighbours: {via: [phone_hash], where: {created_within: 7d}}, transform: {cap: 20}}
rules:
  - {name: invite_spam, mode: shadow, scorer: local, weights: uniform,
     inputs: [custom.invites_10m_peak, custom.external_invite_share_24h, custom.invites_blocked_within_10m,
              custom.linked_invite_share_24h, custom.phone_siblings_7d, brand.name_match,
              brand.title_match, core.linked_deleted_n, core.history_truncated],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

`invitee_hash` and `blocker_hash` share `join_domain: member`. The same member id therefore hashes
identically in both types, and `sequence.on` can join them.

**Declarative:** everything above. **Go-only:** classifying message text. The product emits that
as `content.verdict`.

### 7d. Developer API: credential stuffing through customer API keys ("Keyforge")

The pattern: a customer's API key drives many end-user login attempts across many distinct
usernames. Most fail, with the occasional success shortly after a failure on the same username.

```yaml
packs: [core@1]
vocabulary:
  version: 1
  subject_kinds: {account: {}, api_key: {parent: account}}
  resource_kinds: {api_key: {role: credential}}
  types:
    auth.attempted:
      role: activity
      fields:
        outcome:     {kind: enum, values: [succeeded, failed, locked]}
        login_hash:  {kind: hash, join_domain: login}
        client_ip24: {kind: hash}
        client_asn:  {kind: enum, values: [residential, mobile, hosting, unknown]}
features:
  - {name: custom.distinct_logins_10m, version: 1, subject_kinds: [api_key],
     description: distinct usernames tried, distinct: {type: auth.attempted, field: login_hash},
     window: 10m, transform: {log1p: true, cap: 8}}
  - {name: custom.failures_1h, version: 1, subject_kinds: [api_key, account], description: failed logins,
     count: {type: auth.attempted, where: {field: outcome, eq: failed}}, window: 1h, transform: {cap: 10000}}
  - {name: custom.attempts_1h, version: 1, subject_kinds: [api_key, account], description: all logins,
     count: {type: auth.attempted}, window: 1h, transform: {cap: 10000}}
  - {name: custom.failure_ratio_1h, version: 1, subject_kinds: [api_key, account], description: failed ÷ all,
     ratio: {num: custom.failures_1h, den: custom.attempts_1h, if_empty: 0}, transform: {cap: 1}}
  - {name: custom.success_after_failure_10m, version: 1, subject_kinds: [api_key],
     description: successes shortly after a failure on the same username,
     sequence: {a: {type: auth.attempted, where: {field: outcome, eq: failed}},
                b: {type: auth.attempted, where: {field: outcome, eq: succeeded}},
                within: 10m, on: {a: login_hash, b: login_hash}},
     window: 24h, transform: {log1p: true, cap: 6}}
  - {name: custom.hosting_share_1h, version: 1, subject_kinds: [api_key], description: attempts from hosting networks,
     share: {type: auth.attempted, match: {field: client_asn, eq: hosting}}, window: 1h, transform: {cap: 1}}
  - {name: custom.max_attempts_per_ip_1h, version: 1, subject_kinds: [api_key], description: busiest client network,
     count: {type: auth.attempted, group_by: {field: client_ip24, reduce: max, max_groups: 1000}},
     window: 1h, transform: {log1p: true, cap: 9}}
  - {name: custom.attempts_vs_history, version: 1, subject_kinds: [api_key], description: attempts vs own past,
     count: {type: auth.attempted}, window: 1h,
     relative_to_history: {lookback: 30d, exclude_recent: 24h}, transform: {log1p: true, cap: 6}}
rules:
  - {name: stuffing_key, mode: shadow, scorer: local, weights: uniform, applies_to: [api_key],
     inputs: [custom.distinct_logins_10m, custom.failure_ratio_1h, custom.success_after_failure_10m,
              custom.hosting_share_1h, custom.max_attempts_per_ip_1h, custom.attempts_vs_history,
              core.history_truncated], labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

```json
{"id":"kf-9001","subject":"key_example_k3","subject_kind":"api_key","type":"auth.attempted","at":"2031-09-04T11:00:01Z","data":{"outcome":"failed","login_hash":"lg_8c7b6a5f4e3d21","client_ip24":"ip_1f2e3d4c5b6a77","client_asn":"hosting"}}
```

**Declarative:** everything above. `client_asn` is an enum the product classifies. **Go-only:**
whether a username appears in a breach corpus (this needs an external lookup), and ASN reputation
finer than the product's own enum.

### 7e. AI inference: free-tier farming ("Lumenloop")

The pattern: many free accounts share a device or an OAuth identity. Each one exhausts its free
token quota soon after sign-up, favours the most expensive models, and is then abandoned.

```yaml
packs: [core@1]
vocabulary:
  version: 1
  link_kinds: {oauth_sub_hash: {evidence: true}}
  types:
    usage.recorded:
      role: activity
      fields:
        model_tier:   {kind: enum, values: [small, medium, large]}
        tokens:       {kind: number, min: 0, integer: true}
        quota_state:  {kind: enum, values: [ok, near_limit, exhausted]}
features:
  - {name: custom.signup_to_quota_exhausted_min, version: 1, description: minutes to exhaust free quota,
     time_between: {from: {type: subject.created}, to: {type: usage.recorded, where: {field: quota_state, eq: exhausted}},
                    until_now: false, if_absent: 10080}, transform: {cap: 10080}, prior_sign: "-"}
  - {name: custom.large_model_token_share_24h, version: 1, description: tokens spent on large models,
     share: {type: usage.recorded, match: {field: model_tier, eq: large}, sum: {field: tokens, default: 0, cap_each: 200000}},
     window: 24h, transform: {cap: 1}}
  - {name: custom.tokens_10m_peak, version: 1, description: tokens in busiest 10 min,
     peak: {type: usage.recorded, sum: {field: tokens, default: 0, cap_each: 200000}, size: 10m},
     window: 24h, transform: {log1p: true, cap: 15}}
  - {name: custom.minutes_since_last_use, version: 1, description: idle time after last use,
     time_between: {from: {type: usage.recorded, anchor: last}, to: {type: abusekit.never}, until_now: true, if_absent: 0},
     transform: {cap: 10080}}
  - {name: custom.oauth_siblings_deleted, version: 1, description: deleted accounts sharing the OAuth identity,
     neighbours: {via: [oauth_sub_hash, device_hash], where: {deleted: permanent}}, transform: {cap: 20}}
  - {name: custom.oauth_siblings_new_7d, version: 1, description: accounts sharing identity created this week,
     neighbours: {via: [oauth_sub_hash, device_hash], where: {created_within: 7d}}, transform: {cap: 20}}
rules:
  - {name: free_tier_farm, mode: shadow, scorer: local, weights: uniform,
     inputs: [custom.signup_to_quota_exhausted_min, custom.large_model_token_share_24h, custom.tokens_10m_peak,
              custom.oauth_siblings_deleted, custom.oauth_siblings_new_7d, core.first_funding_prepaid,
              core.linked_deleted_n, core.history_truncated],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

`abusekit.never` is a reserved type that never occurs. With `anchor: last` and `until_now`,
`time_between` becomes "minutes since the last event of type A". That gives the idle-time
primitive with no new op.

`custom.minutes_since_last_use` is deliberately left out of the rule. Abandonment only means
something alongside the neighbour counts, and uniform priors can't express that combination. The
feature is kept for hand-tuned weights later.

**Declarative:** everything above. **Go-only:**
- prompt-content similarity across accounts, which needs cross-subject text clustering and a text
  scorer;
- feature interactions ("abandoned **and** has farmed siblings"). A uniform-prior logistic model
  can't capture these; they need tuned or fitted weights, or a multiplicative custom feature. §12
  Q15 asks whether `ratio` should gain a `product` form.

### 7f. What the walk shows

| Need | Primitive |
| --- | --- |
| Per-entity maxima and counts (per card, per link, per IP) | `group_by` |
| Cause, then effect within a time limit (fail → success, invite → block, signup → exhaustion) | `sequence`, `time_between` |
| Rates | `ratio` |
| Cross-account farms | Declared link kinds + `neighbours` |
| Non-account actors (cards, API keys) | Subject kinds + `also` + `parent` |

**Still Go-only across all five:**
- content understanding (files, messages, prompts);
- external reputation lookups;
- text similarity across subjects;
- non-linear feature interactions under uniform priors.

The first two already have a channel: products emit `content.verdict` or enum fields.

## 8. Migration plan for e2a

- **Emitter.** No change. S6 emits `content.sent` and the rest of main §4.12 as designed.
- **Profile.** e2a's private profile starts as a byte copy of `examples/tenants/reference/`:
  - `packs: [core@1, email@1, brand@1]`;
  - the implicit legacy vocabulary, made explicit: `key: credential` with #7's aliases, and
    `agent: other`;
  - `new_account_velocity` with namespaced inputs.

  The weights move to the private mount. The private brand list stays private (`brand.extra`).
  Floors for e2a's real corpus live privately; the public reference floors stay here.
- **Golden (P0).** `abusekit eval --golden out.jsonl` extends the existing eval replay rather than
  adding a new command. It runs on `main` after #5 and #7 merge, and its output is committed as
  `eval/golden/reference-flat.jsonl`. For every fixture, subject, event instant and scheduled
  rescore instant, it records:
  - feature values, as `Float64bits`;
  - `NextRescoreAt`;
  - per-rule input hashes, risks and tiers;
  - the score;
  - the local `Version()`.
- **Rename (P1).** P1 re-baselines under the semantic-identity rules of criterion 1:
  - identical feature bits under the rename map;
  - identical `NextRescoreAt` and tiers;
  - `|Δrisk| ≤ 1e-12`, with no score near a cut point;
  - every hash recorded as changed, exactly once.

  The result is committed as `eval/golden/reference-ns.jsonl`. Every later slice asserts
  **exact** equality with that file.
- **Other one-time baseline changes, each isolated in its own slice:**
  - the `(at, producer, id)` tie order, in P0, before capture;
  - re-HMAC of hash values and links, in P3: feature values unchanged, stored bytes changed;
  - the domain kind at `RedactionSchemaVersion` 3, also in P3. Fixtures use `.test`, so nothing
    changes.
- **Rollback.** Before S8, each slice can be reverted on its own. After S8, the rename can't be
  undone without re-scoring, which is why it lands first.

## 9. Slices

These come after #5 and #7 merge. The early slices are small, and pack gating arrives only after
the machinery exists.

| # | Slice | Contents | Depends on | Done when |
| --- | --- | --- | --- | --- |
| P0 | Golden replay | `abusekit eval --golden`; scoring loader ordered `(at, producer, id)`; `eval/golden/reference-flat.jsonl` | #5, #7 | Golden committed; test green; flipping one weight's last bit fails it |
| P1 | One-time rename | Every §5.2 consumer, each with its test; the `FeatureDef` metadata table (quantum, bound, prior sign, truncation direction, reads); `core.Vector`; reason v2; the corpus key-space column and migration; the cassette header; `feature_renamed`; stage gate limited to advise-mode local rules | P0 | `reference-ns.jsonl` meets criterion 1; the grep test finds no flat literals; all existing suites green |
| P2 | Tenant profiles | Private-mount loader; `examples/tenants/reference`; per-tenant atomic reload and `/healthz`; per-tenant rule sets (`computeVerdict`, `currentRuleNames`); per-tenant local scorer and version; per-tenant fair queue and concurrency cap. Every built-in feature is available to every tenant. | P1 | Golden exact; tenant-isolation tests (rules, reload, view); fairness test |
| P3 | Declared types and kind redaction | `internal/vocab`; `internal/secret` (`Keys`, file adapter); field kinds; roles (`credential`/`other`, `activity`, `title`, `self`); `x_` extensions; the PSL-based domain kind; card/IP/phone scan; re-HMAC of all hash fields and links, with `join_domain`; undeclared values dropped; skeleton-only custom text; `vocab_version`; config history plus `abusekit config check` | P2 | Criterion 5 property tests; golden exact (features unchanged); `vocab_incompatible` and history CI tests |
| P4a | DSL core: `count`, `distinct`, `share`, `peak` | Compiler; windows (`window`, `first`, `lifetime`); predicates; transforms; caps and limits; the bounded loader (§5.7) and step budget; `core.history_truncated` with its truncation invariant; end-to-end benchmark and `cost_table.yaml` | P3 | Reference equality on 10k histories for these four ops; criterion 4 at P4a limits; criterion 6 flood property; loader fuzz |
| P4b | DSL extended: `time_between`, `before_first`, `relative_to_history`, `group_by`, `sequence`, `ratio` | Plus proportional rescore coalescing, the per-tenant rescore budget, and warm-up | P4a | Reference equality for every op; every "expressible" #7/S2 feature equals its Go twin bit for bit; warm-up test |
| P5 | Pack gating | `internal/pack` registry; `core`/`email`/`brand` adapters (code moved); enablement and dependency validation; `brand.title_match`; `packtest`; starter weights | P2 (P4a for `packtest`'s flood check) | Golden exact; `feature_not_enabled`; every pack passes `packtest` |
| P6a | Link kinds, `neighbours`, subject kinds | `links.custom`; `neighbours`; `subject_kind`, `also`, `parent`; `event_subjects`; `?kind=`; `applies_to`; derived `x_primary_subject_hash` | P4a, P5 | Contract tests for kinds and `also`; neighbour caps; golden exact |
| P6b | Scenarios and bootstrap | The five `examples/tenants/*` profiles, with dev and held-out fixtures; uniform priors; `--profile`; corpus v2; floors with `profile:` | P4b, P6a | Criterion 2 on held-out fixtures; the held-out isolation CI check |
| P7 | e2a cutover | e2a's profile in the ops repo's private mount (outside this repo); hosted-config CI runs `abusekit config check`; the `rules.yaml` path removed | P5 (and S8's mount) | Golden exact against the private copy; the hosted deploy loads it |

- **Ordering against the v0 plan.** P0 and P1 must land before S5 and S8, because assumption A1
  is what makes a rename without a bridge safe.
- **S3b (erasure)** must be vocabulary-aware (§5.10), and is easiest to build after P3.
- **S6** is independent of every P slice.

## 10. Scalability and extensibility

- **Per-subject cost** is bounded by bytes and steps, not by event count. The loader fetches only
  as far back as the profile's largest lookback, capped at 8 MiB of history plus 1 MiB of
  onboarding events. Compiled plans are cached per `profile_sha`.
- **Tenants.** The fair queue and the per-tenant concurrency cap let about 100 tenants share one
  worker pool without starving each other. Metrics are labelled `{tenant, pack}`.
- **Rescores.**
  - Timer rescores come only from features that feed non-shadow rules.
  - Their coalescing buckets scale with the feature's window.
  - Each tenant has an hourly budget.
- **Neighbours.** At most 4 queries per extraction; each is indexed, fan-in capped, and cached for
  the extraction.
- **Made easier later:**
  - new Go packs, gated by `packtest`;
  - promoting a popular custom feature into a pack upstream;
  - weight fitting on corpus v2;
  - CEL as a `where` leaf;
  - cross-tenant linking, which this design leaves untouched.

## 11. Verification strategy

Tests sit at the seams callers actually cross:
- the profile loader: profiles in, errors out;
- `LoadHistory` + `feature.Extract`: history in, vector out;
- `POST /v1/events`: redaction;
- `abusekit eval --profile`.

The checks:
1. **Golden replay.** Criterion 1 in P1, then exact equality in every later slice.
2. **`packtest`** for every pack (§5.9).
3. **DSL conformance.** The naive reference against the compiled evaluator. Table tests for each
   op's edge cases, clipping, `before_first` inclusivity, `track_max`, and the group and key caps.
4. **Redaction property tests.**
   - Mask versus reject, by kind.
   - Re-HMAC, including `join_domain` equality and separation.
   - Undeclared values dropped.
   - Domain PSL and IP checks.
   - Luhn, IP and phone detection, with explicit false-positive fixtures.
5. **Loader fuzzer**, with the §5.7 limits as the oracle, plus config-history CI tests.
6. **Performance and flooding.** The end-to-end benchmark (criterion 4) and the flood property
   (criterion 6).
7. **Tenant isolation.** Rules, reload, view, the fair queue and the rescore budget.
8. **HTTP contract tests.** `subject_kind`, `also`, `links.custom`, `?kind=` and `feature_renamed`.
9. **Most likely regressions, and what catches each:**

| Regression | Caught by |
| --- | --- |
| A stage lookup missed by the rename | grep test + stage test |
| A lost hash quantum | hash-drift test |
| Tie order | golden |
| A weights edit that breaks the truncation invariant | `packtest` |
| PSL snapshot drift | pinned version + test |

## 12. Open questions (owner decisions)

Where the review's answer differs from revision 1's recommendation, both are shown.

1. **Bridge vs rename.** Revision 1: a frozen canonical-key bridge. Review, and now recommended:
   rename once in P1 and re-baseline under semantic identity. Reason: nothing stored needs a
   bridge yet (A1), and a bridge would keep two names alive forever. Approve?
2. **Ordering.** Revision 1: G0–G3 before S5 and S8. Now: P0 and P1 **must** land before S5 and
   S8, because the no-bridge rename depends on it. Approve?
3. **Undeclared data.** Revision 1: keyed-hash undeclared strings. Review, and now: drop every
   undeclared value and keep only type, time and field names. Reason: the hash of an unreviewed
   field is still personal data. Approve?
4. **Built-in re-HMAC.** Re-HMAC `recipient_hash` and every `links` value at ingest. Stored bytes
   change; feature values don't. Approve?
5. **Legacy window quirks.** Freeze them in `@1` and harmonise in `@2` (recommendation unchanged).
   Confirm?
6. **Roles.** Revision 1: five resource roles. Review, and now: only `credential` and `other`,
   plus the `activity` type role and the `title`/`self` field roles. Approve?
7. **Load bounds.** Revision 1: the 50,000 newest events and a 250 ms deadline. Review, and now: a
   time-bounded load, onboarding types in full, byte caps of 8 MiB and 1 MiB, a calibrated step
   budget, and truncation as a positive signal. Confirm the defaults?
8. **Bootstrap.** Uniform priors, shadow only, no fitting, and held-out fixtures. Confirm?
9. **CEL.** Add it later as a `where` leaf only, or require a new design pass? (Unchanged.)
10. **Brand list.** May a tenant narrow the list as well as extend it? (Unchanged.)
11. **Custom namespace.** `custom.*` per tenant, or `<tenant>.*`? (Unchanged.)
12. **Config history.** Revision 1: history held in the DB. Review, and now: history in the config
    tree, checked in CI, with the DB as a runtime guard only; atomic reload per tenant. Approve?
13. **What S6 emits.** Revision 1 offered a choice between `content.sent` and `delivery.sent`.
    Review, and now: `delivery.sent` is dropped, so S6 emits `content.sent`. Settled unless you
    object.
14. **Domain kind.** Require a PSL suffix or an RFC 6761 special-use name, and reject IP literals
    and all-numeric labels, **including on built-in domain fields** (`RedactionSchemaVersion` 3).
    Declared fields also get an optional `reduce: etld1`. Approve?
15. **Feature interactions.** Should `ratio` gain a `product` form (depth-1 DAG, capped) for the
    interactions that §7e shows uniform priors can't capture, or should that wait for fitted
    weights?
16. **Subject kinds and `also`.** Add the wire fields `subject_kind` and `also` (at most 3), with
    subjects keyed `(tenant, kind, id)`. Both are additive. Approve?
17. **Stage-gate fix.** Stage gates consider only advise-mode local rules. This changes behaviour
    on `main`. Approve?
18. **Profiles and secrets.** Tenant profiles live in a private mount, with only fictional
    examples in the repo, and the key interface is provider-agnostic. Approve?
19. **Rescore control.** Proportional coalescing applies to DSL features only (built-ins keep
    5 minutes for semantic identity). Only non-shadow rules schedule timer rescores, under a
    per-tenant hourly budget. Confirm the default of 20 × active subjects per hour?

## 13. Changes from revision 1

**Blockers:**
- **B1:** the canonical-key bridge is dropped. The rename happens once, with a test for every
  consumer it touches (§5.2), and the golden checks semantic identity against a derived
  floating-point bound.
- **B2:** the event-count bound and the wall-clock deadline are replaced (§5.7) by:
  - a time-bounded load, with onboarding types loaded in full;
  - byte caps and a deterministic step budget;
  - truncation as a positively weighted signal, with a checked invariant and an argument that
    flooding can't evade.
- **B3 (§5.6):**
  - every hash is re-HMACed at ingest, with length prefixes and `join_domain`;
  - undeclared values are dropped;
  - domains are checked against the PSL and rejected if they are IP literals;
  - the leak scan covers card, IP and phone shapes;
  - custom text is stored skeleton-only;
  - stored values are described as pseudonymised throughout.
- **B4:** new primitives `group_by`, `sequence`, `ratio`, `neighbours` with declared link kinds,
  `before_first`, `anchor: last`, and subject kinds with `also` and `parent`. All five scenarios
  are walked, and what remains Go-only is stated (§7).

**Should-fix:**
- `delivery.sent` is dropped in favour of declared types with `title`/`self` roles and an
  `activity` type role; `x_` extension fields are added.
- Warm-up, and dual-key rotation.
- Exact `relative_to_history` equations, with a list of what the DSL can't express.
- A benchmark-calibrated cost table and a per-type fan-out cap.
- Scheduling: rescore-storm control, a fair queue, and stage gates limited to advise-mode rules.
- Config history in the config tree; profiles in a private mount.
- `(at, producer, id)` tie-breaks; held-out fixtures; `PriorSign` on `FeatureDef`; per-tenant
  rule names.
- Re-slicing into P0 → P7; S3b erasure made vocabulary-aware.

**Nits:**
- Only the `credential`/`other` resource roles.
- A provider-agnostic `Keys` interface.
- A per-tenant scorer version.
- `peak` clipping specified.
- Uniform-prior normalisation written out.
