# Generic feature packs, a neutral event vocabulary, and declarative custom features

Status: proposed, 2026-09-29 · owner: Josh Zhang · amends
[`2026-09-27-abusekit-design.md`](2026-09-27-abusekit-design.md) (§4.2, §4.3, §4.5, §4.6, §4.10).
Written against `main` at S3 plus the two open PRs treated as merged: #5 (S4 evaluation harness)
and #7 (S2b send-volume, webmail, recipient and subject-brand features). Section numbers below
refer to this document; `main §x` refers to the main design.

## 1. Problem statement

abusekit's current feature set assumes an email platform. The main design promises a scoring
service for any product that mints accounts, takes payments and lets users create resources. The
built code does not keep that promise:

- **The vocabulary is email-shaped.** `content.sent` carries `subject_line`, `recipient_domain`
  and `recipient_is_own_identity`, fields that only mean something for mail. Resource kinds are an
  undeclared convention: `internal/feature` counts `kind == "key"` (plus S2b's spelling aliases)
  and treats everything else as a generic resource.
- **Half the features are email features.** Of the 25 features on `main` + #7,
  `first_day_distinct_domains`, `self_send_before_external`, `sends_10m_max`, `sends_1h`,
  `sends_first_day`, `webmail_recipient_share`, `webmail_sends_1h`, `distinct_recipients_1h` and
  `subject_brand_match` read `content.sent` and only mean something for mail. `key_velocity_1h`
  and `key_total` assume API keys. A file-sharing, payments or chat product would get these at
  zero.
- **Custom event types are dead weight.** Main §4.3 says unknown types are "stored, available to
  Go-registered features only". No Go feature reads them, so the only way for a product to add a
  signal is to write Go in this repo.
- **Everything is global.** One `config/rules.yaml`, one `config/local_weights.yaml` and one flat
  feature namespace serve every tenant. A second tenant can't turn features on or off, and a new
  feature name can collide with an existing one.

**Desired outcome.** A product that is not an email platform can onboard with YAML only. It
enables the packs that fit its domain, declares its event types and field kinds, defines its
product-specific signals as declarative features, and runs in shadow. The first consumer (e2a)
keeps identical scores, bit for bit.

### Success criteria (measurable)

1. **Bit-identical migration.** A golden replay covers every committed fixture
   (`eval/fixtures/*.jsonl`, `eval/fixtures/synthetic/`, and #7's fixtures). It scores after every
   event and at every scheduled rescore instant. Each recorded feature value, `NextRescoreAt`,
   per-rule input hash, risk (compared as `math.Float64bits`), tier and local-scorer `Version()`
   must be identical before and after every slice in §9. The same holds for the harness's
   `run.json` metrics once timestamps and git sha are removed.
2. **Zero-Go onboarding.** The three fictional products in §7 load from a tenant YAML file with
   no Go changes. With uniform priors (§5.9), each product's abusive fixture scores above every
   one of its benign fixtures.
3. **Enablement is enforced.** For a tenant without the `email` pack, no `email.*` feature is
   computed, stored or rendered. A rule that references one fails the config load with
   `feature_not_enabled`.
4. **Declarative features are correct and bounded.** For every operation in §5.5, the compiled
   evaluator equals a naive O(n²) reference implementation on 10,000 randomized histories,
   including shuffled arrival order and future-dated events. Per-subject extraction p99 is ≤ 50 ms
   on 2 vCPU for a tenant at the limits in §5.7: 64 custom features over a history of 50,000
   events.
5. **Privacy by construction.** A property test shows that no stored value of a declared `text`
   field matches the email-shape matcher. Values in undeclared fields are stored only as keyed
   hashes, and config can never introduce a regex or executable code. The loader fuzz test finds
   no profile that passes validation and breaks a limit in §5.7.

## 2. Goals and non-goals

**Goals**
- Split the built-in features into three packs: `core` (product-neutral), `email`, and `brand`
  (display-name impersonation). Each pack is registered in Go and enabled per tenant.
- Namespace every feature (`core.subject_age_h`, `email.sends_10m_max`). Keep a frozen alias
  table for today's flat names, so stored verdicts, corpora, cassettes, floors and weights stay
  valid.
- Add a neutral delivery event (`delivery.sent`) and product-declared resource kinds, channels
  and custom event types. `content.sent` stays accepted forever.
- Add declarative custom features in YAML. The set is closed: count, distinct, share, peak,
  time-between, and a history-relative modifier. Every feature carries a mandatory cap and
  transform, has deterministic semantics, and is validated and versioned at load.
- Per-tenant redaction for declared fields, driven by field kinds.
- Per-pack starter weights, fixtures, floors and golden-sign/mutation tests. A shadow-only
  uniform-prior mode for a tenant that has no labels yet.

**Non-goals**
- Arbitrary code or expressions in config: no CEL, no regex, no WASM, no Go plugins (§5.12).
- Learning weights from labels (`abusekit fit`). Bootstrapping stays in shadow until an operator
  hand-tunes weights or a later design adds fitting (§12 Q8).
- Cross-tenant feature sharing or linking. Main §2 already defers this.
- Changes to scoring math. `core.Plan`, `core.Combine` and the local logistic model are
  unchanged. Only their feature keys and quantization metadata move to a table (§5.2).
- A runtime API for declaring vocabularies. Declarations are reviewed config, like rules (§5.6).

## 3. Relevant context and constraints

**Code this design must fit (on `main` + #5 + #7):**
- `internal/event`: structural `Validate`, plus a static redaction `schema` map keyed by type.
  Its `RedactionSchemaVersion` is 2 after #7. Unknown types keep every key after a recursive
  leak scan; unlisted keys of known types are dropped.
- `internal/feature`: a monolithic `Extract(ctx, tenant, subject, events, neighbors, windows,
  brands, webmail)` returns one fixed struct, `Features`, with 25 fields and a hand-written
  `Map()`. `Names` feeds `config.FeatureSet`. `nextRescoreAt` hard-codes the windowed types
  (`resource.created`, `content.sent`).
- `internal/core`: `inputHash` JSON-encodes the rule's feature map keyed by name.
  `quantizeAgeFeaturesForHash` special-cases `subject_age_h` and `upgrade_delay_min` **by name**.
- `internal/model/local`: sums `weight × feature` in **sorted feature-name order** (S10). Its
  `Version()` is a SHA-256 of the JSON-encoded `Weights`, whose weight map is keyed by name.
  Renaming a feature therefore changes both the floating-point summation order and the version
  hash. The migration must neutralise both (§5.2).
- `eval` (#5): corpus-v1 rows carry `input.features` keyed by flat name. Cassettes are keyed on
  `(scorer, scorer_version, model, prompt_version, input_hash)`. `floors.yaml` entries are keyed
  on `(rule, scorer, slice)`. `eval/gen` generates the synthetic corpus.
- Worker and config: rules and weights are global, and keys already carry a `tenant`.

**Patterns to reuse:** load-time validation that rejects the whole document (main §4.5). Data
files loaded at boot (`brands.yaml`, `webmail.yaml`). Pure core with injected dependencies. The
compare-and-clear worker queue. Expand-only migrations.

**Assumptions** (unconfirmed ones are repeated in §12):
- A1. Neither vendor adapter (S5) nor the hosted deploy (S8) has shipped. No production verdicts
  and no vendor cassettes exist yet. The design still keeps them stable (§5.2) in case the order
  changes.
- A2. At most about 100 tenants and about 64 custom features per tenant. Retention of 90 days
  for event text (main §4.11) bounds any lookback.
- A3. Products can compute keyed hashes for identifiers they want to count distinctly, as e2a
  already does for `recipient_hash`. As a fallback, abusekit hashes undeclared fields with its
  own per-tenant key (§5.6).

## 4. Proposed design: overview

```
                   tenant profile (config/tenants/<tenant>.yaml)
                   ├─ packs: [core@1, email@1, brand@1]
                   ├─ vocabulary: resource kinds, channels, custom types + field kinds
                   ├─ features: declarative custom.* definitions
                   ├─ rules: inputs reference namespaced features
                   └─ weights: per rule (file, or `uniform`)
                                  │ compile + validate (per tenant, atomic)
                                  ▼
ingest ──▶ vocab.Redact(tenant) ──▶ store (wire form + vocab_version)
                                           │
worker ──▶ vocab.View (content.sent → delivery view) ──▶ feature.Extract(profile)
                                           │  for each enabled pack, in fixed order:
                                           │    core (Go) · email (Go) · brand (Go) · custom (compiled YAML)
                                           ▼
                             Vector{values, canonical keys, quanta}
                                           │
                             core.Plan / Combine (unchanged math)
```

| Module | Interface | Deletion test |
| --- | --- | --- |
| `internal/vocab` (new) | `Compile(builtin, decl) (*Vocabulary, error)`; `(*Vocabulary).Redact(*event.Event) error`; `(*Vocabulary).View(event.Event) event.View` | Without it, redaction, kind declarations and the `content.sent` → delivery mapping spread across ingest and every pack. Keep. |
| `internal/pack` (new) | `Pack` interface (§5.1) + registry; adapters `core`, `email`, `brand` (Go) and `custom` (compiled YAML) | Four adapters, so the seam is real. Without it, `feature.Extract` stays a monolith that only grows. |
| `internal/pack/custom` (new) | `Compile(tenant, []Def, *Vocabulary) (Pack, error)` | Holds all DSL semantics. Deleting it leaves products writing Go again. Keep. |
| `internal/feature` | `Extract(ctx, profile, subject, events, now) (Result, error)`: runs enabled packs, merges, min-reduces rescore instants | Becomes a thin orchestrator. It stays because the worker, evaluate and eval all call exactly this one function. |
| `internal/config` | `LoadProfiles(dir, deps) (map[tenant]*Profile, []error)` | Absorbs `rules.yaml`; per-tenant atomic reload. |
| `internal/core` | unchanged signatures; `Plan` takes `Vector` instead of `map[string]float64` | The only change is that canonical keys and quanta come from data (§5.2). |

The existing S2b brand, webmail and window helpers move into the packs that own them, with their
code unchanged. That is what keeps the migration bit-for-bit.

## 5. Proposed design: detail

### 5.1 Packs: registration, enablement, dependencies

A **pack** is a named, versioned bundle of feature definitions and computation. It may also
carry data files (brand lists, webmail lists), starter weights, fixtures and floors. Packs are
compiled into the binary and registered in `internal/pack/registry.go`. A tenant **enables**
packs; it never supplies pack code.

```go
// Pack computes a namespaced group of features for one subject.
type Pack interface {
    ID() ID                      // {Name: "email", Version: 1}
    Requires() []string          // packs whose features/views it reads, e.g. email@1 → [core]
    Features() []FeatureDef      // static metadata, see below
    // Extract is pure: same Input → same Output, independent of event order.
    // It must exclude events with At > Input.Now from every window (§5.5 notes one frozen
    // legacy exception).
    Extract(ctx context.Context, in Input) (Output, error)
}

type FeatureDef struct {
    Name        string   // "email.sends_10m_max"; grammar §5.3
    Legacy      string   // frozen flat alias ("sends_10m_max"), "" for features born namespaced
    HashQuantum float64  // input-hash bucket; 0 = exact (§5.2)
    Bound       float64  // max value after transform; used by uniform priors (§5.9)
    Reads       []string // view types read ("delivery", "resource.created", ...), for dispatch
                         // and rescore scheduling
    RequiresPacks []string // feature-level dependency, e.g. email.subject_brand_match → [brand]
}

type Input struct {
    Tenant, Subject string
    Events   []event.View   // stored events projected through the tenant vocabulary
    Now      time.Time
    Start    time.Time      // account start: subject.created.account_created_at if present,
                            // else earliest event At (S2b R8)
    Neighbors NeighborEvidence // resolved once by the orchestrator, only when core is enabled
    Params   PackParams     // this tenant's validated per-pack settings (lists, channels, kinds)
}

type Output struct {
    Values  map[string]float64 // exactly the names in Features(); missing = bug, rejected
    Rescore []time.Time        // candidate instants at which some value changes with no new event
}
```

**Enablement** lives in the tenant profile: `packs: [core@1, email@1, brand@1]`. The rules are:
- `core` is always enabled and is implied if omitted. Every other pack is opt-in.
- A pin names a major version. A pack changes feature semantics only by shipping a new major
  version (`email@2`) next to the old one for at least one release. Semantic changes never
  happen in place. Every verdict records the pinned versions in `profile_sha` (§5.10).
- `Requires` must be satisfied by the enabled set. Otherwise the tenant fails to load with
  `pack_requires`.
- A feature whose `RequiresPacks` are not all enabled is **not registered** for the tenant. For
  example, `email.subject_brand_match` needs `brand`; enable `email` without `brand` and that one
  feature doesn't exist for the tenant.
- The orchestrator runs packs in fixed order: `core`, `email`, `brand`, then `custom`. It merges
  their `Values` and takes the earliest `Rescore` instant, coalesced to the existing 5-minute
  buckets. No two packs share a namespace, so collisions are impossible by construction.

**Pack contents after the split** (legacy name → namespaced name):

| Pack | Features |
| --- | --- |
| `core@1`, account and onboarding | `subject_age_h`→`core.subject_age_h` (quantum 1) |
| `core@1`, payment | `upgrade_delay_min`→`core.upgrade_delay_min` (quantum 60), `upgraded`→`core.upgraded`, `declines_before_first_success`→`core.declines_before_first_success`, `first_funding_prepaid`→`core.first_funding_prepaid`, `fingerprint_seen_on_other_subjects`→`core.fingerprint_seen_on_other_subjects` |
| `core@1`, resource velocity | `resource_velocity_1h`→`core.resource_velocity_1h`, `resource_total`→`core.resource_total`, `key_velocity_1h`→`core.credential_velocity_1h`, `key_total`→`core.credential_total` (a declared kind with `role: credential`, §5.4) |
| `core@1`, linked subjects and neighbour evidence | `linked_deleted_n`→`core.linked_deleted_n`, `neighbors_truncated`→`core.neighbors_truncated` |
| `core@1`, labels | `linked_labelled_abusive_n`→`core.linked_labelled_abusive_n` |
| `core@1`, behaviour change | `burst_ratio_24h_vs_lifetime`→`core.burst_ratio_24h_vs_lifetime`. Its activity set is `resource.created` ∪ delivery views, which is exactly today's `resource.created` ∪ `content.sent`. The shared `burstFactor`/`ageDecayFactor` functions are also exposed to the DSL as `relative_to_history` (§5.5). |
| `email@1`, requires `core` | `sends_10m_max`, `sends_1h`, `sends_first_day`, `webmail_recipient_share`, `webmail_sends_1h`, `distinct_recipients_1h`, `first_day_distinct_domains`, `self_send_before_external`, `subject_brand_match` (the last also requires `brand`). All become `email.<same>`. They read delivery views whose `channel` is in the pack's `channels` param (default `[email]`). |
| `brand@1` | `name_brand_match`→`brand.name_match`, `name_has_at`→`brand.name_has_at`; new `brand.title_match` (§5.1.1). Data: `brands.yaml` plus the tenant's optional private `extra` list (S2b `brands_extra`). |

The `core.credential_*` rename is the only name that moves beyond adding a prefix. "Key" is an
e2a word; "credential" is the product-neutral role (§5.4). The legacy alias keeps e2a's hash and
weight keys unchanged.

New core features are **additive**. They are registered but not in e2a's rule, so e2a's golden
replay can't move:
- `core.email_domain_class_disposable`: main §8 Q7 deferred this. It reads the existing
  `subject.created.email_domain_class` and is neutral (sign-up identity quality, not mail sending).
- `core.verdict_max_24h`: the maximum `content.verdict.score` in the trailing 24 h.
- `core.history_truncated`: see §5.7.

#### 5.1.1 The brand pack

The matcher is S2b's `BrandSet`, moved unchanged: NFKC and confusables skeleton, word and token
boundaries, Unicode punctuation tokenizing, case-sensitive entries, the integration and
community gates, and Cf stripping. It applies to any product with user-chosen display names:
workspace names, storefront names, profile names, community names.

- `brand.name_match` and `brand.name_has_at` read `resource.created` and `resource.deleted`
  `name` (and `name_skeleton`) across **all** declared kinds, as they do today.
- `brand.title_match` is the neutral sibling of `email.subject_brand_match`. It counts distinct
  brands in non-self delivery titles over a trailing 1 h, capped at 3, and has none of the
  email-specific exemptions. Non-email tenants use it. e2a keeps `email.subject_brand_match`,
  whose S2b integration-name exemption and double-count rule are specific to mail.
- The brand list is pack data: `config/packs/brand/brands.yaml` (public), plus a per-tenant
  private `brand.extra` path merged at boot with `MergeBrandSets`.

### 5.2 Namespacing and the canonical key (how migration stays bit-for-bit)

Every feature has two identities:
- **Name**: the namespaced name, used everywhere a human or config refers to the feature: rules,
  weights files, corpus v2, reasons, docs.
- **Canonical key**: `Legacy` if the feature has one, else `Name`. It is used in exactly three
  places:
  1. `core.inputHash`: the rule's feature map is re-keyed by canonical key before JSON encoding.
     Quantization comes from each feature's `HashQuantum`, which replaces the name switch in
     `quantizeAgeFeaturesForHash`. `core.subject_age_h` has quantum 1 and
     `core.upgrade_delay_min` has quantum 60, the same values applied under the same keys.
  2. The local scorer's summation order: `sortedFeatures` is sorted by canonical key.
  3. The local scorer's `Version()`: it hashes the weights map re-keyed by canonical key.

For a migrated feature, the canonical key is the flat name today's code uses. Summation order,
version hash, input hashes and therefore cassette keys are byte-identical, so no subject is
rescored at cutover and no vendor call is repeated. A feature born namespaced (every new pack
feature and every `custom.*`) has canonical key = name, so nothing legacy leaks into new work.

**The alias table is frozen.** It is a Go literal of exactly the 25 flat names on `main` + #7,
and CI enforces it: the table can never gain an entry, and no new `Name` may equal any alias.

**Where flat names are still accepted (read-side only):**

| Artifact | Behaviour |
| --- | --- |
| Rules (`inputs`) | Resolved through the alias table, but only if the owning pack is enabled for the tenant. Otherwise the load fails with `feature_not_enabled`. A load warning says the name is deprecated. |
| Weights files | Same resolution. A file must not mix a flat name and its namespaced twin (`duplicate_feature`). |
| Corpus v1 rows | `eval.LoadSnapshotCorpus` maps `input.features` keys through the table. An unknown flat key fails the load. Export always writes corpus-v2 (§5.9). |
| Text inputs | Aliases `subject_line_skeleton`→`delivery.title_skeleton` and `first_link_host`→`delivery.link_host`. Their canonical keys stay the legacy names, for the same hashing reason. |
| Floors | Entries with no `profile:` default to `tenant:e2a` (§5.9). |
| Stored verdicts | Untouched. They store `input_hash`, risk and reason, and never feature names. |

### 5.3 Name grammar and reserved namespaces

- Feature names: `^[a-z][a-z0-9]*\.[a-z][a-z0-9_]{0,55}$`, at most 64 bytes.
- Reserved feature namespaces: `core`, `email`, `brand`, `custom`, plus any future pack name.
  Tenants may define only `custom.*`. The `custom.` namespace is per tenant, so two tenants may
  each have a `custom.invites_1h` that means different things (§12 Q11).
- Event type names keep the wire grammar `^[a-z_.]+$`, at most 64 bytes, and must contain a dot.
  Reserved type prefixes (built-ins, tenants may not declare types under them): `subject.`,
  `payment.`, `subscription.`, `resource.`, `content.`, `delivery.`, `abusekit.`.

### 5.4 Neutral event vocabulary

#### Built-in types after this change

The wire contract (`POST /v1/events`, main §4.3) is unchanged in shape. Every change below is
additive: a new built-in type, new optional fields, and tenant-declared types.

| Type | Change |
| --- | --- |
| `subject.created`, `subject.deleted`, `subject.class`, `payment.attempt`, `subscription.changed`, `content.verdict` | none (keeps #7's optional `account_created_at`) |
| `resource.created` / `resource.deleted` | `kind` is interpreted through the tenant's declared **resource kinds** (below). On the wire it stays free text. |
| `content.sent` | **Legacy email delivery, accepted forever, with #7's redaction unchanged.** Projected to a delivery view (below). |
| `delivery.sent` (new) | Neutral delivery: N recipients at destination D, with optional title text. |

`delivery.sent.data`:

| Field | Kind | Rule |
| --- | --- | --- |
| `channel` | enum, declared per tenant | Optional. A tenant with exactly one declared channel may omit it; otherwise it is required. An undeclared value → `redaction_failed`. |
| `destination` | the kind the channel declares: `domain` \| `hash` \| `enum` | Optional. Where the delivery lands: a recipient domain, a keyed community id, a region code. |
| `destination_class` | enum, declared per channel | Optional, for example `own_community` \| `other_community`. |
| `recipient_count` | positive integer | Optional, default 1 for every feature that sums it. |
| `recipient_hash` | hash (`^[A-Za-z0-9_:+/=-]{8,128}$`) | Optional. Exactly one recipient, so paired with `recipient_count > 1` → `redaction_failed` (#7's rule). |
| `to_self` | bool | Optional. The recipient is the subject's own identity. |
| `title` | text ≤ 200 | Optional. NFKC, email-shaped substrings masked, skeleton stored as `title_skeleton`. |
| `link_host` | domain | Optional. First link host in the delivered content. |

#### The delivery view (read-side projection)

Features never read `content.sent` or `delivery.sent` directly. They read `event.View`, which
`vocab.View` produces from the stored row. For `content.sent`:

| Delivery view field | Taken from `content.sent` |
| --- | --- |
| `channel` | constant `email` |
| `destination` (kind `domain`) | `recipient_domain` |
| `recipient_count` | `recipient_count` |
| `recipient_hash` | `recipient_hash` |
| `to_self` | `recipient_is_own_identity` |
| `title` / `title_skeleton` | `subject_line` / `subject_line_skeleton` |
| `link_host` | `first_link_host` |

A `delivery.sent` row maps to the same view field for field. The email pack's features are the
S2b functions with one mechanical edit: `e.Type != "content.sent"` becomes
`v.Kind != event.ViewDelivery || !channels[v.Channel]`, and the field reads are renamed.
Everything else is untouched, including missing-field defaults, `recipientCountOf`'s default of
1, `normalizeToken` domain folding and `isSelfSend`. So an e2a stream of `content.sent` and the
same stream rewritten as `delivery.sent{channel: email}` yield identical features. §8's
translation test proves this on every fixture.

**Why read-side and not a rewrite at ingest.** Rewriting would change the stored type and the
`BodyHash` that separates `duplicate` from `conflict`. It would also need a data migration for
stored rows and would still leave old rows to interpret. The read-side view needs no migration
and makes both forms equivalent by construction.

#### Product-declared resource kinds and channels

```yaml
vocabulary:
  version: 3                    # monotonically increasing; see §5.6 for compatibility rules
  resource_kinds:
    agent:     {role: identity}
    key:       {role: credential, aliases: [keys, api_key, api_keys, api-key, apikey, "api key"]}
  channels:
    email:     {destination: domain}
```

- `role` is a closed enum: `credential`, `identity`, `workspace`, `content`, `other`.
  `core.credential_*` counts kinds with `role: credential`; `core.resource_*` counts every kind.
  Matching folds case and trims whitespace, then applies `aliases`. This is the S2b behaviour
  moved into data: e2a's declaration above reproduces `resourceKindAliases` exactly, and the
  golden replay proves it.
- An **undeclared kind** is stored, counts in `core.resource_*` (role `other`), and increments
  `abusekit_undeclared_kind_total{tenant}`. That matches today's behaviour for kinds that aren't
  keys. With `vocabulary.strict: true`, an undeclared kind is instead rejected with the existing
  `redaction_failed` code, so no new per-item code is needed.
- A tenant with no `resource_kinds` block gets the **implicit legacy declaration** above. That is
  how today's global config keeps working (§8).

#### Versioning

The wire format stays `/v1`. Every addition is optional, and producers already handle unknown
per-item codes because the code list is closed and unchanged. The one semantic change is to
how **undeclared** types are stored (§5.6). It is flagged, and it is licensed because the
service is pre-GA: main §4.3 documents unknown types as "stored", and nothing reads them yet.
`RedactionSchemaVersion` for built-ins stays at 2 (#7). Each stored row gains
`vocab_version text` (`"<tenant>@<n>"`, NULL for built-in-only schemas) next to
`redaction_version`, in an expand-only migration.

### 5.5 Declarative custom features

#### Schema

```yaml
features:
  - name: custom.public_links_1h          # custom.* only
    version: 1                            # bump on ANY change to the definition (enforced, §5.7)
    description: public share links created in the last hour   # required, shown in reasons
    count:                                # exactly one op key: count | distinct | share | peak | time_between
      type: share.link_created            # a declared type, or a built-in type/view
      where: {field: visibility, eq: public}
      sum: {field: size_class_weight, default: 1, cap_each: 10}   # optional; count = sum of this
    window: 1h
    transform: {log1p: true, cap: 50}     # cap mandatory; log1p optional; applied log1p → cap
```

**Windows.** Either `window: <dur>` (trailing, `(now − dur, now]`) or `first: <dur>` (anchored,
`[start, start + dur)`). `<dur>` is a whole number of minutes, hours or days: `1m`–`30d`.
`lifetime` means `(−∞, now]`. `start` is the account start defined in §5.1 `Input.Start`.

**Predicates (`where`).** A closed set of operators. There is no regex, no arithmetic and no
user functions.

| Leaf | Meaning | Allowed field kinds |
| --- | --- | --- |
| `{field: f, eq: v}` / `{field: f, ne: v}` | equality after the kind's normalisation | enum, bool, number, domain, hash |
| `{field: f, in: [..]}` / `not_in` | membership, ≤ 256 values; for enums every value is checked against the declared enum at load, so a typo is a load error | enum, number, domain, hash |
| `{field: f, in_set: <name>}` | membership in a tenant- or pack-provided set file (for example, `email`'s webmail list), hashed at load; ≤ 100,000 entries | domain, hash, enum |
| `{field: f, suffix_in_set: <name>}` | domain-label-boundary suffix match (`a.b.example.test` ⊂ `example.test`), linear time | domain |
| `{field: f, gte: n}` / `lte` / `gt` / `lt` | numeric compare | number |
| `{field: f, exists: bool}` | presence | any |
| `{all: [..]}` / `{any: [..]}` / `{not: leaf}` | combinators, depth ≤ 2, ≤ 8 leaves in total | |

An absent field or a type mismatch makes a leaf false; `{exists: false}` is the only leaf that
is true on absence. `text` fields can't appear in `where` or as a `distinct` field. Text is only
available to text-accepting scorers through a rule's `text:` list. That keeps free text out of
every aggregation.

**Operations.**

| Op | Value at `now` |
| --- | --- |
| `count` | The number of matching events in the window, or, with `sum`, the sum of the field over them. `default` covers absence, and `cap_each` clamps each event's addend before summing, as S2b does with `recipient_count`. |
| `distinct` | `{type, where, field}`: the number of distinct normalised values of `field` among matching events in the window. `field` must be an `enum`, `number`, `domain` or `hash` field. Tracking stops at `track_max`, the smallest count whose transformed value reaches `transform.cap` (`cap` itself without `log1p`, `ceil(expm1(cap / scale))` with it). The loader rejects a definition whose `track_max` exceeds 10,000, so memory is O(track_max). |
| `share` | `{type, where (denominator), match (numerator predicate), sum?}`: numerator ÷ denominator over the window; `if_empty` (default 0) when the denominator is 0. |
| `peak` | `{type, where, sum?, size: <dur>}`: the maximum count or sum in any window `(t − size, t]` with `t` an event instant inside the outer window. `size` ≤ window and window ÷ size ≤ 1440. A two-pointer scan over the time-sorted matches. |
| `time_between` | `{from: {type, where}, to: {type, where}, until_now: bool, if_absent: n}`: minutes from `t_A` (the earliest matching `from`) to `t_B` (the earliest matching `to` with `t_B ≥ t_A`). No `from` event → `if_absent`. A `from` but no `to` → minutes since `t_A` when `until_now`, else `if_absent`. `if_absent` is mandatory. Negative results can't occur, and the value is floored at 0 as a guard. |

**The history-relative modifier.** It may be added to `count`, `distinct` and `peak`, and it
generalises S2b's `burstFactor × ageDecayFactor`:

```yaml
    relative_to_history:
      lookback: 30d            # ≤ 30d
      exclude_recent: 24h      # the current burst never serves as its own baseline
      age_decay: {full_until: 3d, zero_at: 30d, floor: 0.2}   # optional; these are the defaults
```

`baseline` is the maximum of the same op, with the same width and predicate, over sliding
windows whose end lies in `(now − lookback, now − exclude_recent]`. The value is then
`min(current / max(baseline, 1), transform.cap)`. If `age_decay` is set, the result is
multiplied by `clamp(1 − (age_days − full_until)/(zero_at − full_until), floor, 1)`. With the
defaults, `full_until = 3d` and `zero_at = 30d` make the denominator 27, which is exactly S2b's
`ageDecayFactor`. `floor > 0` is mandatory, so a decayed signal is never a hard zero.

**Transform.** `cap` is mandatory for every feature: a finite value > 0, and at most the op's
natural bound (1 for `share`). `log1p` is optional and takes one of three forms, applied before
the cap: `log1p: true` gives `ln(1 + v)`; `log1p: {scale: s}` gives `s · ln(1 + v)`; and
`log1p: {anchored_at: n}` sets `s = n / ln(1 + n)`, so `v = n` maps to `n`. The last form
reproduces `first_day_distinct_domains`'s S2b shape. The transformed value always lies in `[0, cap]`, and
that is the feature's `Bound`.

#### Evaluation semantics

- **Pure and order-independent.** A feature is a function of the set of stored events and
  `now`, never of arrival order. Where "first" is ambiguous, ties at the same instant break by
  `(at, event id)`. Late events bump `dirty_seq` as they do today.
- **Half-open windows.** Trailing windows are `(now − W, now]`; anchored windows are
  `[start, start + W)`; `peak` sub-windows are `(t − S, t]`.
- **Future events are excluded everywhere,** `lifetime` included: an event with `at > now`
  contributes to no custom feature until `now` reaches it. It does contribute a rescore
  candidate at its own `at` (the S2b B4 behaviour, generalised).
  *Frozen legacy exception:* the migrated Go features `core.resource_total` and
  `core.credential_total` count future-dated events in their lifetime totals (the S2 behaviour),
  and `email.first_day_distinct_domains` uses an inclusive `[start, start + 24h]`. Both are kept
  for bit-for-bit parity and documented in each feature's docstring. Harmonising them is a
  `core@2`/`email@2` change (§12 Q5).
- **Rescore candidates.** For each windowed feature, the compiler emits: the exit instant of the
  oldest in-window match (`at + W`); anchored window ends still in the future; for `peak`, the
  exit of the current maximum's sub-window; for `relative_to_history`, the instants where an
  event crosses `now − exclude_recent` or `now − lookback`; and every future-dated match. The
  orchestrator takes the minimum and coalesces it to the 5-minute bucket. This generalises
  `isWindowedEventType`: the set of windowed types is the union of every feature's `Reads`.
- **Determinism of floats.** Sums accumulate in time order `(at, id)` with a single `float64`
  accumulator, so the same event set always gives the same bits.

#### Compilation and cost model

`pack/custom.Compile` validates each definition and produces a **per-tenant plan**: an index
from view type to the list of (feature, predicate program) pairs that read it. Extraction makes
one pass over the subject's events, dispatches each event to the features for its type, and
evaluates the flat predicate programs. `peak` and `relative_to_history` keep per-feature
matched-instant slices, and a final pass per feature runs the two-pointer scans.

Static cost units, checked at load:

| Op | Units |
| --- | --- |
| `count`, `share`, `time_between` | 1 |
| `distinct`, `peak` | 2 |
| `relative_to_history` | ×2 on top of the op's own cost |
| Each predicate leaf beyond the first | +0.25 |

Per-tenant budget: **256 units**. With the 50,000-event scan bound (§5.7), the worst case is
about 50,000 events × 8 dispatched features per type × 8 leaves ≈ 3.2M predicate steps, plus
O(n) scans. That is tens of milliseconds, which is what criterion 4 measures. At runtime a
per-subject extraction deadline (default 250 ms) applies to the custom pack. If it expires, the
pack fails as described in §6.

### 5.6 Redaction for declared types and fields

Ingest never consults rules (main §4.3). It consults the tenant **vocabulary**, which is
config, reviewed like code, and compiled into `vocab.Vocabulary`. The built-in `schema` map
becomes the built-in half of every vocabulary, unchanged.

```yaml
vocabulary:
  version: 1
  types:
    share.link_created:
      fields:
        visibility: {kind: enum, values: [public, org, private]}
        file_kind:  {kind: enum, values: [document, archive, executable, image, other]}
        size_bytes: {kind: number, min: 0, integer: true}
        folder_title: {kind: text, max_len: 120, skeleton: true}
    share.downloaded:
      fields:
        link_hash:        {kind: hash}
        downloader_ip24:  {kind: hash}
        downloader_is_owner: {kind: bool}
```

**Field kinds and the rule for each:**

| Kind | Accepts | On violation |
| --- | --- | --- |
| `text` | string; NFKC; control characters and invalid UTF-8 rejected; **email-shaped substrings masked to `@`** (#7's `subject_line` rule, generalised); truncated at `max_len` (≤ 500, default 200); optional `skeleton` companion | reject for control characters or bad UTF-8; mask for an email shape |
| `number` | finite float64; optional `min`, `max`, `integer` | reject |
| `bool` | bool | reject |
| `enum` | string in `values` (≤ 64 values, each ≤ 64 bytes, `[a-z0-9_.-]+`) | reject |
| `hash` | `^[A-Za-z0-9_:+/=-]{8,128}$` (no `@`, `%` or whitespace) | reject (never truncated) |
| `domain` | lower-cased, IDNA to ASCII, hostname grammar (labels 1–63, total ≤ 253); a user part is impossible by grammar | reject |
| `timestamp` | RFC 3339 (#7's `account_created_at` pattern) | reject |

**Rules that make it privacy by construction:**
1. The existing recursive leak scan runs first, over every key and value of every event.
   Email-shaped content outside a `text` field is still rejected. Declared `text` fields are the
   only exemption, and they are masked instead of rejected.
2. **Undeclared fields of a declared type are hashed, not stored.** A string becomes
   `hk1:` + hex(HMAC-SHA256(tenant redaction key, type ‖ field ‖ value))[:32]. Numbers and bools
   pass through. Objects and arrays are dropped. A hashed field can be used only in
   `distinct`, `eq`/`in` on a hash (the producer would have to compute the same HMAC, which it
   can't, so in practice only `distinct` and `exists`). It can never be read back.
3. **Undeclared types** get the same treatment, with every top-level key treated as undeclared.
   This replaces "kept as-is" (§5.4 versioning note). Undeclared types remain unusable by
   features until they are declared.
4. **The redaction key** is a per-tenant secret held by abusekit (Secret Manager,
   `abusekit-<tenant>-redaction-key`). It is separate from the producer-held link-hash key.
   Rotating it breaks equality across the rotation boundary for hashed fields, which is
   acceptable because they only feed windowed `distinct`. The key id is recorded in
   `vocab_version`.
5. Caps: ≤ 32 declared types per tenant, ≤ 32 fields per type, `data` ≤ 8 KiB after redaction
   (unchanged).

**Vocabulary compatibility.** Stored rows are immutable, and features must be able to read old
rows. So a vocabulary may only **widen**: add a type, add a field, add enum values, raise
`max_len` or `max`, or switch `strict` off. Changing a field's kind, removing or narrowing enum
values, or lowering a cap requires a new field name. The loader enforces this against the latest
accepted vocabulary for the tenant, recorded in a new `tenant_vocabularies(tenant, version,
sha, body, accepted_at)` table. The version must increase with any change, and an incompatible
change fails the load with `vocab_incompatible`. The harness reports how many rows were stored
under each `vocab_version`.

### 5.7 Limits (validated at load; also the fuzz oracle)

| Limit | Value |
| --- | --- |
| Custom features per tenant | 64 |
| Cost units per tenant | 256 |
| Predicate depth / leaves per feature | 2 / 8 |
| `in` list size / set file entries | 256 / 100,000 |
| Window, lookback | ≤ 30 d; `lifetime` allowed only for `count`, `distinct`, `share`, `time_between` |
| `peak` window ÷ size | ≤ 1440 |
| `distinct` cap | ≤ 10,000 |
| Events scanned per subject | 50,000 newest by `(at, id)`, plus the earliest event and `subject.created` for `start` |
| Extraction deadline (custom pack) | 250 ms default, per-tenant override ≤ 1 s |

Above the scan bound, the orchestrator sets `core.history_truncated = 1`. No committed fixture
comes near the bound, so the golden replay is unaffected. Custom-feature versioning: the loader
keeps a SHA-256 of each normalised definition per `(tenant, name, version)` in
`tenant_feature_defs`. Redefining an existing `(name, version)` with a different body fails with
`feature_version_reused`.

### 5.8 Rules and validation at load

A tenant profile is `config/tenants/<tenant>.yaml`:

```yaml
tenant: e2a
packs: [core@1, email@1, brand@1]
pack_params:
  brand: {extra: /run/secrets/brands_extra.yaml}   # optional, private
  email: {channels: [email], webmail_set: default}
vocabulary: {...}          # §5.4, §5.6
features: [...]            # §5.5
tiers: {medium: 0.4, high: 0.8}
min_scored_advise: 1
rules:
  - name: new_account_velocity
    mode: advise
    scorer: local
    weights: tenants/e2a/weights.yaml      # or `uniform` (shadow only, §5.9)
    inputs: [core.subject_age_h, ...]
```

Validation adds these checks to main §4.5's list. The whole tenant profile is rejected with a
collected error list, and the codes are machine-readable in `/healthz`:
- `pack_unknown`, `pack_requires`, `pack_version_unknown`
- `feature_unknown`: a name exists in no pack and not in the tenant's `custom.*` set
- `feature_not_enabled`: the name, or its alias, exists but its pack or `RequiresPacks` isn't
  enabled
- `feature_namespace`: a tenant tries to define a feature outside `custom.*`
- `duplicate_feature`, `feature_version_reused`
- `dsl_invalid` (with a JSON-pointer path): an unknown op, a missing cap, a text field in a
  predicate, an enum value not declared, a window out of range, too many leaves, a cost overrun
- `vocab_invalid`, `vocab_incompatible`
- `weights_unknown_feature`: every weight must name an input of the rule it serves
- `uniform_not_shadow`: a rule that uses `weights: uniform` must be in `mode: shadow`

**Reload is atomic per tenant.** A rejected profile keeps that tenant's previous profile live
and has no effect on other tenants; `/healthz` reports `config_error{tenant}`. This replaces
today's whole-config rejection, which would let one tenant's typo freeze every tenant's rule
changes. Before any tenant file exists, `config/rules.yaml` + `config/local_weights.yaml` load
as the **default profile**. That profile has implicit `packs: [core@1, email@1, brand@1]`, the
implicit legacy vocabulary, and flat-name resolution, and it applies to every tenant that has
keys but no file. This is the zero-change path for e2a until slice G7 (§9).

### 5.9 Weights, scorers, eval and bootstrap per pack

- **Weights are per rule**, keyed by namespaced name (flat names are accepted through the alias
  table). The scorer name stays `local`. At profile compile time the loader binds a local scorer
  instance per weights file. `Version()` is content-derived over canonical keys (§5.2), so
  different weights produce different versions automatically. Registry lookups become
  `(tenant, scorer)`, and vendor scorers stay global.
- **The local scorer's math is unchanged:** `sigmoid(bias + Σ w·x)`, summed in canonical-key
  order.
- **Starter weights per pack.** Each pack ships `config/packs/<pack>/starter.yaml`: one rule,
  `<pack>_starter`, with its own bias and weights over that pack's features only. Every weight
  carries `sign: +|-` (the golden-sign contract). Starter rules compose: a tenant can enable
  `core_starter` and `brand_starter` as separate shadow rules, and `Combine`'s
  `max(risk)` handles them without inventing a joint model. The `core` starter is derived from
  today's e2a weights restricted to core features. The `email` and `brand` starters are derived
  the same way. All are placeholders until labelled data exists for a second product.
- **Uniform priors, shadow only.** `weights: uniform` binds a local scorer with, for `k` inputs,
  `x̂ᵢ = xᵢ / Boundᵢ ∈ [0, 1]` and `wᵢ = ±4/k` on `x̂ᵢ`. The sign defaults to `+`, and a feature
  can declare `prior_sign: -` (for example, an account-age or time-to-first-action feature). The
  bias is `−2 + (4/k) × (number of negative-sign inputs)`, so risk always ranges from
  sigmoid(−2) ≈ 0.12 to sigmoid(2) ≈ 0.88. The result is a ranking device
  for operator review, never a tier driver. The loader rejects it in `advise`
  (`uniform_not_shadow`), and promotion (main §4.5) requires a real weights file plus a gate run.
- **Eval is scoped to a profile.** `abusekit eval --profile pack:email` or
  `--profile tenant:e2a` selects the rule, weights, fixtures and floors. Layout:
  `eval/packs/<pack>/{fixtures/, floors.yaml}` and `eval/tenants/<tenant>/{fixtures/,
  floors.yaml, golden/}`. `floors.yaml` entries gain `profile:`; a missing value means
  `tenant:e2a`, so #5's file keeps working unchanged. The manifest gains `profile`,
  `profile_sha` and `pack_versions`.
- **Cassettes** are unchanged. Their key already includes `input_hash`, and canonical keys keep
  it stable (§5.2).
- **Corpus v2** (`eval/schema/corpus-v2.schema.json`) is v1 plus a required `profile` and
  `vocab_version`, with `input.features` keyed by namespaced name. `LoadSnapshotCorpus` reads
  both versions.
- **Golden-sign and mutation tests per pack** become a reusable harness,
  `internal/pack/packtest.Run(t, pack)`, and every pack must pass it in CI, like the adapter
  contract test. It checks:
  1. Every starter weight's sign matches `sign:`.
  2. Zeroing each weight moves at least one of the pack's fixture bands or an isolated scenario
     (today's `mutation_test.go` logic, parameterised).
  3. Determinism: same bits under shuffled event order and repeated runs.
  4. No leakage from the future: adding an event at `now + ε` changes no value except rescore
     candidates.
  5. Every emitted name is in `Features()` and in the pack's namespace, with `Bound` respected.

  e2a's tenant profile keeps its own golden-sign and mutation suite over its composed rule, with
  namespaced keys.
- **How a second product bootstraps:**
  1. Enable packs, declare the vocabulary, and write custom features. Run shadow rules
     `core_starter` (plus `brand_starter` if relevant) and a `custom_uniform` rule over the
     custom features.
  2. Collect labels through `POST /v1/labels` (main §4.9). Corpus rows accrue per profile.
  3. Once a labelled set passes the harness, hand-tune a real weights file, set floors, and
     promote through the normal shadow → advise path. Fitting is §12 Q8.

### 5.10 Provenance and storage changes (expand-only)

- `events.vocab_version text NULL`.
- `verdicts.profile_sha text NULL`: SHA-256 of the pack versions, the normalised custom-feature
  definitions and the vocabulary version. It is recorded, not part of the input hash, so
  editing an unrelated custom feature doesn't force rescoring. A changed value still changes the
  hash through the value.
- New tables `tenant_vocabularies` and `tenant_feature_defs` (§5.6, §5.7).
- No change to `links`, `subjects`, `labels` or `corpus_examples`.

### 5.11 API surface summary

| Surface | Change | Compatibility |
| --- | --- | --- |
| `POST /v1/events` | `delivery.sent` built-in; tenant-declared types redacted by kind; undeclared fields hashed | Additive on the wire. Storage of undeclared types changes (pre-GA, §5.4). |
| `GET /v1/subjects/{id}`, `evaluate` | Signal `reason` templates may name namespaced features in prose | Additive; the shape is unchanged. |
| Per-item codes | none new (`redaction_failed`, `bad_type` reused) | unchanged |
| Config YAML | tenant profiles; `rules.yaml` still loads as the default profile | Backward compatible. Flat names are deprecated with a warning. |
| Weights, floors, corpus files | namespaced keys; `profile:`; corpus-v2 | v1 read forever via the alias table |
| `pkg/abusekit` client | `DeliverySent` event helper; no removals | additive |

**Rejected API alternative:** a `PUT /v1/vocabulary` endpoint that would let producers declare
schemas at runtime. It would let a producer key widen its own redaction boundary, which is a
privilege escalation. It would also move a privacy decision out of code review.

### 5.12 Alternatives considered

- **CEL (cel-go) for predicates and features.** It is sandboxed, has cost estimation, and is a
  known quantity. It lost for four reasons:
  1. Features aggregate over time-windowed sequences of events. CEL has no windowed aggregates,
     so we would still have to write count, distinct, peak, time-between and history-relative as
     custom functions. CEL would only wrap the predicate, which is the easy part.
  2. It pulls in a large dependency (cel-go plus protobuf), against the repo's minimal-dependency
     convention.
  3. CEL's cost estimate is per expression. Ours has to be per subject history, which needs our
     own model anyway.
  4. Its error messages and semantics (`has()`, dynamic types) are harder for a product engineer
     to get right than a closed YAML schema whose load errors carry JSON pointers.

  CEL remains the fallback **for `where` only** if the closed predicate set proves too small
  (§12 Q9).
- **A home-grown expression language.** It would bring a parser, a grammar, precedence rules and
  an injection surface, all needing a security review, for no coverage beyond the six closed
  operations the target signals need.
- **A plugin ABI.** Go `plugin` needs an identical toolchain and build flags and has no sandbox.
  WASM (wazero) is sandboxed with fuel metering, but it deploys arbitrary code disguised as
  config, reviewers can't read it, and float determinism depends on the guest. Products that
  need code contribute a Go pack upstream. The `Pack` seam is where that code goes, and
  `packtest` gates it.
- **SQL-defined features against the store.** They couple to the schema, their cost is
  unbounded, and they are a tenant-isolation hazard. Rejected.
- **Keep flat names and prefix only new features.** No migration, but also no enablement
  boundary, and two naming styles forever. Rejected in favour of the canonical-key bridge, which
  costs one frozen table.
- **Translate `content.sent` to `delivery.sent` at ingest.** See §5.4. Rejected for the
  body-hash and migration costs.
- **Rename everything and rescore once.** This gives a simpler hash with no canonical keys. It
  lost because summation order changes the last bits of risk (breaking the bit-for-bit
  criterion), and any vendor cassette recorded before cutover would go stale.

## 6. Edge cases and failure handling

- **A pack's `Extract` errors or the custom pack exceeds its deadline.** The orchestrator
  records the failure per pack, not per subject. Rules whose inputs include any feature of that
  pack become `unscored` with `error_code: feature_error` or `feature_timeout`, and
  `degraded: true` is set. Rules that don't read the pack still score. This fails closed:
  `unknown` or `degraded`, never `low` by absence (main §5). A pack that fails for every subject
  of a tenant pages through the existing metric.
- **A profile is rejected on reload.** The previous profile stays live for that tenant only.
  On a cold start with no valid profile, the tenant's subjects stay unscored (`unknown`) and
  `/healthz` is red. The service never falls back to a different tenant's rules.
- **Events of a type arrive before its declaration (deploy ordering).** They are stored under
  the undeclared-type rule (strings hashed). Features declared later can't read those strings,
  but they can still count the events. The runbook says to declare first, and the harness
  reports the row counts per `vocab_version`.
- **A declared kind or channel is missing on an event.** Undeclared kinds count as `other`, as
  in §5.4. For channels, `delivery.sent` with an undeclared channel is rejected
  (`redaction_failed`), because silently counting it under a guessed channel would corrupt the
  email features.
- **Absent optional fields in custom features.** Predicates are false. `sum` uses `default`.
  `share` with a zero denominator uses `if_empty`. `time_between` with no `from` event uses
  `if_absent`. There is never a NaN: the compiler proves every op total, and the orchestrator
  rejects a non-finite value as a pack error.
- **Duplicates and out-of-order events.** Ingest idempotency is unchanged. Features are
  set-functions with `(at, id)` tie-breaks.
- **Clock skew and future events.** Excluded from custom windows, but they schedule a rescore
  at their `at`. The frozen legacy exceptions are listed in §5.5.
- **Disabling a pack that rules still reference.** The load fails with `feature_not_enabled`.
  Past verdicts stay; they are provenance.
- **A tenant enables `email` for a non-email channel.** `email.channels` must name declared
  channels whose `destination` kind is `domain`. Otherwise the load fails (`pack_params_invalid`).
  This stops webmail and domain logic running over hashes.
- **Alias misuse.** A rule lists both `sends_1h` and `email.sends_1h` → `duplicate_feature`. A
  custom feature named after an alias is impossible because of the `custom.` prefix.
- **Hostile config.** There is no code and no regex. Every string set is hashed at load. Every
  size is capped. The loader is fuzzed with the §5.7 limits as the oracle.
- **Hostile events against a custom feature.** An attacker can't exceed the per-subject scan or
  deadline bounds. Flooding one subject raises only that subject's cost, which the existing
  per-subject budgets cap. `distinct` memory is O(cap).
- **Brand pack on a product whose display names are routinely brand-adjacent**, such as a
  marketplace reselling branded goods. `brand.*` stays shadow until that tenant's own labels
  justify a weight. That tenant can also point `brand.extra` at an empty list and a narrowed
  public list through `pack_params.brand.list` (§12 Q10).

## 7. Worked examples (fictional)

All three products, their names, ids and domains are invented. Timestamps use the fictional
2031 convention.

### 7a. File sharing: malware-distribution burst ("Driftbox")

The pattern: a fresh account uploads an executable or archive, creates many public share links
quickly, and those links are downloaded from many distinct networks within an hour.

```yaml
tenant: driftbox
packs: [core@1, brand@1]            # no email pack: Driftbox doesn't deliver mail
vocabulary:
  version: 1
  resource_kinds:
    workspace: {role: workspace}
    api_token: {role: credential}
    folder:    {role: content}
  types:
    share.link_created:
      fields:
        visibility: {kind: enum, values: [public, org, private]}
        file_kind:  {kind: enum, values: [document, archive, executable, image, other]}
        size_bytes: {kind: number, min: 0, integer: true}
    share.downloaded:
      fields:
        link_hash:           {kind: hash}
        downloader_ip24:     {kind: hash}     # producer-keyed hash; never a raw IP
        downloader_is_owner: {kind: bool}
features:
  - name: custom.public_links_1h
    version: 1
    description: public share links created in the last hour
    count: {type: share.link_created, where: {field: visibility, eq: public}}
    window: 1h
    transform: {log1p: true, cap: 6}
  - name: custom.risky_file_link_share_24h
    version: 1
    description: share of new links pointing at executables or archives
    share:
      type: share.link_created
      match: {field: file_kind, in: [executable, archive]}
    window: 24h
    transform: {cap: 1}
  - name: custom.distinct_downloader_nets_1h
    version: 1
    description: distinct downloader /24 networks, excluding the owner
    distinct:
      type: share.downloaded
      where: {field: downloader_is_owner, eq: false}
      field: downloader_ip24
    window: 1h
    transform: {log1p: true, cap: 9}          # track_max = ceil(expm1(9)) = 8103 ≤ 10,000
  - name: custom.download_peak_10m_vs_history
    version: 1
    description: 10-minute download peak relative to the account's own past
    peak: {type: share.downloaded, where: {field: downloader_is_owner, eq: false}, size: 10m}
    window: 24h
    relative_to_history: {lookback: 30d, exclude_recent: 24h, age_decay: {}}
    transform: {log1p: true, cap: 6}
  - name: custom.signup_to_first_public_link_min
    version: 1
    description: minutes from sign-up to the first public link
    time_between:
      from: {type: subject.created}
      to:   {type: share.link_created, where: {field: visibility, eq: public}}
      until_now: true
      if_absent: 1440
    transform: {cap: 1440}
    prior_sign: "-"                            # faster = riskier
rules:
  - name: core_starter
    mode: shadow
    scorer: local
    weights: packs/core/starter.yaml
    inputs: [core.subject_age_h, core.credential_velocity_1h, core.resource_velocity_1h,
             core.declines_before_first_success, core.first_funding_prepaid,
             core.linked_deleted_n, core.linked_labelled_abusive_n, core.burst_ratio_24h_vs_lifetime]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.6
  - name: malware_burst
    mode: shadow
    scorer: local
    weights: uniform
    inputs: [custom.public_links_1h, custom.risky_file_link_share_24h,
             custom.distinct_downloader_nets_1h, custom.download_peak_10m_vs_history,
             custom.signup_to_first_public_link_min, brand.name_match]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.7
```

Sample events:

```json
{"id":"db-001","subject":"acct_example_db_1","type":"subject.created","at":"2031-03-02T09:00:00Z","links":{"email_hash":"<64-hex>"},"data":{"channel":"signup","email_domain_class":"disposable"}}
{"id":"db-002","subject":"acct_example_db_1","type":"resource.created","at":"2031-03-02T09:01:10Z","data":{"kind":"workspace","name":"Official Document Center"}}
{"id":"db-003","subject":"acct_example_db_1","type":"share.link_created","at":"2031-03-02T09:03:00Z","data":{"visibility":"public","file_kind":"archive","size_bytes":812345}}
{"id":"db-004","subject":"acct_example_db_1","type":"share.link_created","at":"2031-03-02T09:03:20Z","data":{"visibility":"public","file_kind":"executable","size_bytes":402112}}
{"id":"db-005","subject":"acct_example_db_1","type":"share.downloaded","at":"2031-03-02T09:05:02Z","data":{"link_hash":"lk_4f1c9a0e7b2d","downloader_ip24":"ip_9a1b2c3d4e5f","downloader_is_owner":false}}
```

The benign counterpart fixture: an older workspace sharing documents with an organisation. Its
links are `org`-visibility, with a few downloads from two networks.

### 7b. Payments or marketplace: card testing ("Tallyport")

The pattern: a merchant account (the subject) pushes many small charge attempts across many
distinct cards, most of them declined, in short bursts.

```yaml
tenant: tallyport
packs: [core@1, brand@1]           # core also scores the merchant's own onboarding payments
vocabulary:
  version: 1
  resource_kinds:
    api_key:  {role: credential}
    storefront: {role: workspace}
  types:
    charge.attempted:
      fields:
        outcome:      {kind: enum, values: [succeeded, declined, blocked]}
        decline_code: {kind: enum, values: [insufficient_funds, do_not_honor, incorrect_cvc, expired_card, fraudulent, other]}
        amount_minor: {kind: number, min: 0, integer: true}
        card_hash:    {kind: hash}
features:
  - name: custom.declines_10m_peak
    version: 1
    description: largest number of declined charges in any 10 minutes today
    peak: {type: charge.attempted, where: {field: outcome, eq: declined}, size: 10m}
    window: 24h
    transform: {log1p: true, cap: 7}
  - name: custom.distinct_cards_1h
    version: 1
    description: distinct cards charged in the last hour
    distinct: {type: charge.attempted, field: card_hash}
    window: 1h
    transform: {log1p: true, cap: 7}
  - name: custom.small_charge_share_1h
    version: 1
    description: share of charges at or under 2.00 in minor units
    share: {type: charge.attempted, match: {field: amount_minor, lte: 200}}
    window: 1h
    transform: {cap: 1}
  - name: custom.decline_share_1h
    version: 1
    description: share of charges declined
    share: {type: charge.attempted, match: {field: outcome, in: [declined, blocked]}}
    window: 1h
    transform: {cap: 1}
  - name: custom.cvc_declines_vs_history
    version: 1
    description: CVC/expiry declines this hour vs the merchant's own past
    count:
      type: charge.attempted
      where: {all: [{field: outcome, eq: declined}, {field: decline_code, in: [incorrect_cvc, expired_card]}]}
    window: 1h
    relative_to_history: {lookback: 30d, exclude_recent: 24h, age_decay: {}}
    transform: {log1p: true, cap: 6}
rules:
  - name: card_testing
    mode: shadow
    scorer: local
    weights: uniform
    inputs: [custom.declines_10m_peak, custom.distinct_cards_1h, custom.small_charge_share_1h,
             custom.decline_share_1h, custom.cvc_declines_vs_history, core.credential_velocity_1h]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.7
```

```json
{"id":"tp-101","subject":"acct_example_tp_7","type":"charge.attempted","at":"2031-06-10T02:14:01Z","data":{"outcome":"declined","decline_code":"incorrect_cvc","amount_minor":100,"card_hash":"cd_1a2b3c4d5e6f"}}
{"id":"tp-102","subject":"acct_example_tp_7","type":"charge.attempted","at":"2031-06-10T02:14:04Z","data":{"outcome":"declined","decline_code":"expired_card","amount_minor":100,"card_hash":"cd_7f8e9d0c1b2a"}}
{"id":"tp-103","subject":"acct_example_tp_7","type":"charge.attempted","at":"2031-06-10T02:14:09Z","data":{"outcome":"succeeded","amount_minor":100,"card_hash":"cd_0f1e2d3c4b5a"}}
```

The benign counterparts: a storefront with steady larger charges and an ordinary decline rate,
and a storefront whose flash sale has high volume but few distinct-card declines. The second
exercises `relative_to_history`, because the merchant's own past peaks raise the baseline.

`payment.attempt` is **not** used for the charges. In the core vocabulary, `payment.attempt`
means the subject paying the product, and it feeds onboarding facts. Card testing is the
merchant's product activity, so it is a custom type. The example makes that distinction
explicit.

### 7c. Chat or community: spam invites ("Hearthchat")

The pattern: new accounts with brand-like display names send large volumes of invites to
people outside their own communities, with external links in the invite text.

This product uses the **neutral built-in** `delivery.sent`, which is what it is for.

```yaml
tenant: hearthchat
packs: [core@1, brand@1]
vocabulary:
  version: 1
  resource_kinds:
    profile:   {role: identity}
    community: {role: workspace}
    bot_token: {role: credential}
  channels:
    invite:
      destination: hash                 # keyed community id
      destination_class: [own_community, other_community]
    direct_message:
      destination: hash
features:
  - name: custom.invites_10m_peak
    version: 1
    description: largest invite fan-out in any 10 minutes today
    peak: {type: delivery.sent, where: {field: channel, eq: invite}, sum: {field: recipient_count, default: 1, cap_each: 50}, size: 10m}
    window: 24h
    transform: {log1p: true, cap: 7}
  - name: custom.distinct_invitees_1h
    version: 1
    description: distinct invitees in the last hour
    distinct: {type: delivery.sent, where: {field: channel, eq: invite}, field: recipient_hash}
    window: 1h
    transform: {log1p: true, cap: 7}
  - name: custom.external_invite_share_24h
    version: 1
    description: share of invites to communities the sender doesn't own
    share:
      type: delivery.sent
      where: {field: channel, eq: invite}
      match: {field: destination_class, eq: other_community}
    window: 24h
    transform: {cap: 1}
  - name: custom.linked_invite_share_24h
    version: 1
    description: share of invites carrying an external link
    share:
      type: delivery.sent
      where: {field: channel, eq: invite}
      match: {field: link_host, exists: true}
    window: 24h
    transform: {cap: 1}
  - name: custom.signup_to_first_invite_min
    version: 1
    description: minutes from sign-up to the first invite
    time_between:
      from: {type: subject.created}
      to:   {type: delivery.sent, where: {field: channel, eq: invite}}
      until_now: true
      if_absent: 1440
    transform: {cap: 1440}
    prior_sign: "-"
rules:
  - name: invite_spam
    mode: shadow
    scorer: local
    weights: uniform
    inputs: [custom.invites_10m_peak, custom.distinct_invitees_1h, custom.external_invite_share_24h,
             custom.linked_invite_share_24h, custom.signup_to_first_invite_min,
             brand.name_match, brand.title_match, core.linked_deleted_n]
    labels: [benign, abusive]
    benign_label: benign
    threshold: 0.7
```

```json
{"id":"hc-201","subject":"acct_example_hc_3","type":"resource.created","at":"2031-08-01T18:00:05Z","data":{"kind":"profile","name":"Support Team - Official"}}
{"id":"hc-202","subject":"acct_example_hc_3","type":"delivery.sent","at":"2031-08-01T18:02:11Z","data":{"channel":"invite","destination":"cm_5e6f7a8b9c0d","destination_class":"other_community","recipient_hash":"iv_0a1b2c3d4e5f","title":"You have been selected - claim now","link_host":"claim-prize.example.test"}}
```

The benign counterpart: a community organiser inviting 20 people over an evening to their own
community, with no links. It exercises `external_invite_share_24h` = 0 and a low peak.

**What the three examples demonstrate:** none needs the email pack. All three reuse `core` and
`brand`. Every product-specific signal is declarative. 7c uses the neutral delivery type
directly, and 7a and 7b show that custom types cover what `delivery.sent` doesn't. Slice G6
commits each example as a loadable profile with fixtures, which is success criterion 2.

## 8. Migration plan for e2a

**Emitter: no change required.** e2a keeps emitting `content.sent` and the rest of main §4.12,
and S6 is built as currently specified. Switching to `delivery.sent{channel: email}` later is
optional and equivalent by construction; the translation test (below) proves it.

**Config mapping.** The default profile (§5.8) serves e2a until G7. G7 then commits
`config/tenants/e2a.yaml`:
- `packs: [core@1, email@1, brand@1]`
- `vocabulary`: the implicit legacy declaration from §5.4, made explicit: `agent` →
  `identity`, `key` → `credential` with S2b's aliases, and `email` → `{destination: domain}`.
- `pack_params`: the brand `extra` path (the private list, as `--brands-extra` today) and
  `email.webmail_set: default` (`config/packs/email/webmail.yaml`, moved from
  `config/webmail.yaml`).
- The `new_account_velocity` inputs rewritten through the alias table. The weights file moves
  to `config/tenants/e2a/weights.yaml` with namespaced keys and the same values.
- Floors move to `eval/tenants/e2a/floors.yaml` with `profile: tenant:e2a`, with numbers
  unchanged.

**Proof of identical scores: the golden replay.**
1. **G0 runs first, on the pre-migration code.** It adds `cmd/abusekit golden` (test-only
   build tag), which replays every fixture. Each subject is scored by the real `feature.Extract`
   → `core.Plan` → local scorer → `core.Combine` path after every event instant and at every
   `NextRescoreAt` the replay produces. The command writes `eval/tenants/e2a/golden/v0.jsonl`,
   one row per (fixture, subject, instant):
   `{features: {canonical_key: float64-bits-hex}, next_rescore_at, input_hash{rule},
   risk_bits{rule}, score_bits, tier, local_version}`.
   It also records the harness `run.json` for the synthetic corpus with volatile fields removed.
   Fixtures come from `eval/fixtures/*.jsonl` (including all of #7's), and the generated corpus
   comes from `eval/fixtures/synthetic/`.
2. **Every later slice** runs `TestGoldenReplay_BitIdentical`, which compares exactly, row for
   row, including the row count. Any diff fails CI and prints the first differing feature.
3. **The translation test** rewrites every `content.sent` in every fixture as
   `delivery.sent{channel: email, ...}` (field mapping in §5.4), replays it, and asserts the
   same golden file.
4. **A corpus round-trip** loads the corpus-v1 synthetic corpus, exports it as v2, reloads it,
   and requires identical `run.json` metrics.

**Stored state at cutover, if S8 has already shipped** (assumption A1 says it hasn't):
- Verdict `input_hash`: unchanged (canonical keys), so no subject is rescored and no vendor call
  repeats.
- Local `Version()`: unchanged (canonical keys).
- Cassettes: unchanged keys.
- Rows stored before G5: `vocab_version` is NULL, which reads as the built-in-only schema.

**Rollback.** Every slice up to G7 is behaviour-neutral for e2a, and the golden replay guards
it. G7 is a config move. Reverting it restores the default profile, which yields the same
golden.

## 9. Slices

These fit after #5 and #7 merge. Each is its own PR with the usual review.

| # | Slice | Contents | Done when |
| --- | --- | --- | --- |
| G0 | Golden capture | `cmd/abusekit golden` (test build tag); `eval/tenants/e2a/golden/v0.jsonl` generated from `main` after #5 and #7; `TestGoldenReplay_BitIdentical` | Golden committed, generated by pre-migration code; the test passes on `main`; perturbing one weight's last bit fails it |
| G1 | Canonical keys | `FeatureDef` metadata table (name, legacy, quantum, bound, reads) for today's 25 features; `core.Vector`; `inputHash` over canonical keys with quanta from data (the name switch removed); local scorer order and version by canonical key; alias resolution in rules, weights and corpus loaders; the frozen-table CI check | Golden bit-identical; a rules file in flat names and one in namespaced names load to equal `Config`s; `quantizeAgeFeaturesForHash` deleted |
| G2 | Vocabulary and delivery view | `internal/vocab` (built-in schema moved unchanged); `delivery.sent` built-in; `event.View`; `content.sent` projection; declared resource kinds with roles and aliases (implicit legacy declaration); `pkg/abusekit` `DeliverySent` | Golden bit-identical; translation test green; redaction tests moved and green; `delivery.sent` contract tests (happy, `recipient_hash`+count>1 rejection, undeclared channel rejection) |
| G3 | Packs and tenant profiles | `internal/pack` registry, `core`/`email`/`brand` adapters (code moved, not rewritten); `feature.Extract(profile, …)` orchestrator with per-pack failure isolation; `config/tenants/*.yaml` loader; default profile from `rules.yaml`; per-tenant atomic reload; `/healthz` per tenant; `brand.title_match`; new additive core features | Golden bit-identical; `feature_not_enabled` and `pack_requires` load tests; a tenant with only `core` computes no `email.*`; one tenant's bad profile leaves another's reload applied |
| G4 | Declarative features | `internal/pack/custom` compiler and evaluator, all six ops plus the modifier, predicates, limits and cost model, rescore candidates, `tenant_feature_defs`; naive reference evaluator in tests; property tests; loader fuzz; benchmark | Criterion 4 met (equality on 10k randomized histories; p99 ≤ 50 ms at the limits); a declarative `custom.key_velocity_1h` and `custom.resource_total_lifetime` match their Go twins bit for bit on every fixture (lifetime excluding future events, documented); fuzzing finds no over-limit profile that loads |
| G5 | Declared types and redaction | field kinds; masking of `text`; hashing of undeclared fields and types with the abusekit-held per-tenant key; `vocab_version` column; `tenant_vocabularies` ledger and the widening-only check; `strict` mode | Criterion 5 property tests; `vocab_incompatible` tests; e2a golden bit-identical (e2a declares no custom types) |
| G6 | Per-pack eval and bootstrap | `packtest` harness; per-pack starter weights with `sign:`; pack fixtures and floors; `--profile`; corpus-v2 schema and export; uniform priors plus `uniform_not_shadow`; the three §7 profiles committed as fixtures | Every pack passes `packtest`; `make gate` runs e2a and every pack profile; each §7 abusive fixture outranks its benign fixtures under uniform priors (criterion 2); #5's floors file still loads unchanged |
| G7 | e2a explicit profile | `config/tenants/e2a.yaml`, weights and floors moved; flat-name deprecation warning on; docs updated (main §4.5 and §4.12 pointers) | Golden bit-identical against the explicit profile; the default profile is used by no tenant in the hosted config; the reverting diff also passes golden |

G0 must land before any other G slice. G1 → G2 → G3 are sequential. G4 and G5 can run in
parallel after G3. G6 needs G4. G7 needs G3 (G5 and G6 are optional for it). **Recommended
ordering against the v0 plan:** G0–G3 before S5, so that vendor render templates are born with
namespaced names, and before S8, so that nothing stored needs the bridge in anger. S3b and S6
are independent of all G slices.

## 10. Scalability and extensibility

- **Custom features per tenant.** Bounded by 64 features and 256 cost units. Compiled plans are
  cached per `profile_sha`, and a reload recompiles one tenant only.
- **Extraction cost.** One pass per subject: O(E) dispatch plus O(E) per windowed scan, with E
  ≤ 50,000. `distinct` memory is O(cap). The existing per-subject budget and the priority queue
  are unchanged. What grows is `EventsForSubject`'s load. Later the store can bound the query to
  `max(lookback, window) + exclude_recent`, plus `subjects.first_seen` for `start`. That is a
  store-only change the pack interface already permits, because `Input.Start` is explicit.
- **Tenants.** About 100 tenants × 64 features is a config and plan-cache concern, not a
  database one. Metrics are labelled `{tenant, pack}`, not `{feature}`, to bound cardinality.
- **Made easier later.**
  - A new domain pack (`sms`, `marketplace`) is one Go package plus `packtest`.
  - A popular custom feature can be promoted into a pack upstream, keeping its name through a
    per-pack alias.
  - Cross-tenant linking is untouched by this design.
  - Weight fitting consumes corpus-v2 per profile.
  - CEL for `where` alone, if ever needed, slots in as a new leaf kind.

## 11. Verification strategy

The seams tested are the ones callers cross: the config loader (profiles in, errors out),
`feature.Extract` (events in, vector out), `POST /v1/events` (redaction), and the harness
(`--profile`).

1. The golden replay and the translation test (§8), in every slice's CI.
2. `packtest` for every pack (§5.9).
3. The DSL conformance suite: the naive reference versus the compiled evaluator on randomized
   histories (shuffled order, future events, ties, empty windows, anchored windows before
   `start`); table tests for every predicate leaf and each op's edge (window boundary
   inclusivity, `if_absent`, `if_empty`, cap and log1p order, `distinct` early stop).
4. Loader tests for every §5.8 code, plus the fuzzer, with limits as the oracle.
5. Redaction property tests: mask versus reject per kind; undeclared fields hashed; no email
   shape stored outside masked text.
6. HTTP contract tests: `delivery.sent`, declared types, `strict` mode, per-tenant `/healthz`.
7. A benchmark at the limits (criterion 4).
8. **Most likely regressions:** summation order (caught by golden), a quantum lost for the age
   features (golden input hashes), the S2b alias list drifting in the vocabulary move (golden
   `core.credential_*`), and window-boundary off-by-one in a DSL op (conformance suite).
9. **Manual checks:** load each §7 profile in a local instance, post its sample events, and read
   the shadow signals and reasons.

## 12. Open questions (owner decisions)

1. **Ordering.** Land G0–G3 before S5 (vendor adapters) and S8 (hosted deploy)? Recommended:
   yes.
2. **What S6 emits.** `content.sent` as designed (recommended, no churn), or the neutral
   `delivery.sent{channel: email}`?
3. **Undeclared-type storage change.** Approve moving unknown types from "kept as-is" to
   "strings keyed-hashed" (a pre-GA semantic change, §5.4 and §5.6)?
4. **abusekit-held per-tenant redaction key.** This is a new secret per tenant, separate from
   the producer's link key. Approve?
5. **Legacy window quirks.** `core.resource_total` and `core.credential_total` count
   future-dated events, and `email.first_day_distinct_domains` has an inclusive end. Keep them
   frozen in `@1` and harmonise in `@2` later (recommended), or harmonise now and accept a golden
   diff?
6. **The `credential` rename.** `key_*` → `core.credential_*`: accept the neutral role vocabulary
   (`credential`, `identity`, `workspace`, `content`, `other`)?
7. **Limits.** 64 features, 256 units, 30-day max window, 50,000 events scanned, 250 ms deadline.
   Confirm or adjust.
8. **Bootstrap policy.** Uniform priors are shadow-only, with no fitting in scope. Confirm that
   advise always needs a hand-set or fitted weights file plus a passing gate.
9. **CEL fallback.** Pre-approve CEL for `where` only if the closed predicate set proves
   insufficient, or require a new design pass?
10. **Brand list scope.** Is `config/packs/brand/brands.yaml` one global public list, or may a
    tenant narrow it (`pack_params.brand.list`) as well as extend it (`extra`)?
11. **The custom namespace.** `custom.*` scoped per tenant (proposed), or `<tenant>.*` so names
    are globally unique in logs and corpora?
12. **Per-tenant reload isolation.** Replaces the main design's whole-config rejection. Confirm.
