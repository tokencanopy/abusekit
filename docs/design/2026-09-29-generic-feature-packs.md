# Generic feature packs, product vocabularies, and declarative custom features

Status: proposed, revision 7, 2026-09-29. Revision 7 applies the focused review of revision 6.
Revision 6 applied an owner decision: abusekit is a generic
framework, and **the compiled binary contains no domain-specific knowledge** (no email and
no e2a concepts). Domain knowledge lives only in declared packs (YAML plus data files). Owner: Josh Zhang.

This document amends [`2026-09-27-abusekit-design.md`](2026-09-27-abusekit-design.md), §4.2,
§4.3, §4.5, §4.6, §4.8 and §4.10. It is written against `main` at S3, treating two open PRs as
merged: #5 (the S4 evaluation harness) and #7 (the S2b send-volume, webmail, recipient and
subject-brand features). `main §x` refers to the main design. §13 lists what changed in each
revision.

## 1. Problem statement

abusekit's current feature set assumes an email platform. The main design promises scoring for any
product that mints accounts, takes payments and lets users create resources. The code built so far
doesn't deliver that:
- **Email-shaped vocabulary.** `content.sent` carries `subject_line`, `recipient_domain` and
  `recipient_is_own_identity`. Resource kinds are an undeclared convention.
- **Email-shaped features.** Of the 25 features on `main` + #7, nine are email features and two
  more assume API keys.
- **Custom types go unused.** No feature reads custom types, so a new signal means writing Go in
  this repo.
- **Global, flat configuration.** There is one `rules.yaml`, one `local_weights.yaml`, one feature
  namespace and one subject kind.

**Desired outcome.** A product that isn't an email platform onboards with a private YAML profile.
In it, the product:
1. declares its event types, field kinds, link kinds and subject kinds;
2. enables the packs that fit its domain;
3. defines product-specific signals as declarative features;
4. runs them in shadow.

Three properties hold throughout:
- Every new value is pseudonymised or dropped at ingest.
- Every feature is either exact or explicitly flagged `partial`.
- Flooding a subject with events can't lower its risk.

For e2a, feature values, risks, tiers and rescore times stay bit-identical.

**Governing principle (revision 6).** The binary is domain-neutral. Everything specific to a
product domain is configuration:
- event types such as `content.sent`, and email-shaped fields and link kinds;
- webmail lists;
- email features.

It ships as **reference packs** in YAML and is loaded through the same mechanism as a product's own
custom features. §5.0 defines exactly what stays compiled.

### Success criteria (measurable)

1. **Bit-exact migration.** A golden replay covers every committed fixture, #7's included, plus the
   synthetic corpus. It scores after every event and at every scheduled rescore instant.
   - Across the rename and every later slice, these stay identical under the rename map, compared
     with `math.Float64bits`: every feature value, `NextRescoreAt`, risk, score and tier.
   - These may change, and only in the rename slice: input hashes, cassette keys, fake-scorer
     probabilities, the `run.json` rule and weights SHAs, and the local `Version()`. The golden
     records their before and after values.
2. **Five generic scenarios.**
   - Each scenario in §7 loads as a fictional profile, with zero Go changes for everything §7
     marks as declarative.
   - Held-out fixtures are authored only after the features are frozen.
   - On those fixtures, with uniform priors, every abusive fixture outranks every benign fixture of
     the same scenario.
3. **Enablement enforced.** Features from a pack that isn't enabled are never computed, stored or
   rendered. A rule that references one fails with `feature_not_enabled`.
4. **Correct, independent, bounded.**
   - Every DSL op equals a naive reference implementation on 10,000 randomized histories. The
     histories include shuffled arrival, ties, future-dated events and floods.
   - Changing or removing one feature never changes another feature's value for the same subject.
   - End-to-end p99 is at most 50 ms per subject at the §5.7 limits.
5. **Privacy by construction.**
   - At rest, nothing matches an email, card (Luhn), IP or phone shape, even after NFKC and
     Unicode-digit folding. This is checked on the serialised row before insert. The only
     exception is a masked marker inside a text field.
   - Every hash, every non-allowlisted domain and every non-account subject id is keyed per tenant.
   - No two tenants share a key.
6. **Bounding can't lower risk relative to the unbounded reference.**
   - For every fixture under the §5.7 flood generator,
     `risk(flooded, bounded evaluator) ≥ risk(flooded, unbounded reference)`. If that can't be
     guaranteed for some feature, the verdict carries `degraded`.
   - This is a relative guarantee. A flood can still legitimately move a feature in the reference
     itself. Frozen legacy dilution is the known case: extra activity lowers
     `core.burst_ratio_24h_vs_lifetime`'s share, and extra non-webmail sends lower
     `email.webmail_recipient_share`, in both evaluators alike.
   - **6b.** Neither `start` nor any class F or N onboarding fact moves under either of two floods:
     - a flood of any type other than `subject.created`, from non-backfill producer keys, however
       its `at` is chosen within the skew allowance;
     - 10,000 tiny `blocked` payments.

## 2. Goals and non-goals

**Goals**
- A one-time rename to namespaced features that keeps the golden replay bit-exact (§5.2).
- A domain-neutral binary (§5.0):
  - The email features become a YAML **reference pack**, proven bit-identical to #7's Go features
    and then deleted from Go.
  - A CI check keeps the core neutral.
  - A second, non-email reference pack proves the framework end to end.
- Per-tenant profiles in a private config mount. Only fictional examples and reference packs live
  in this repo.
- Product-declared vocabularies:
  - types, with field kinds and roles;
  - `x_` extension fields;
  - the `activity` role;
  - resource roles;
  - link kinds;
  - subject kinds.
- Pseudonymising redaction:
  - re-HMAC under keys derived per tenant;
  - domain allowlist or HMAC;
  - drop undeclared data;
  - bounded numbers;
  - masked or skeleton-only text;
  - an egress scan.
- A closed DSL covering five scenarios, with mandatory caps and per-feature budgets.
- Evaluation over facts and counters maintained at ingest, plus exact per-feature aggregates.
  Hitting a bound produces a flag, never a weight.
- Packs, each with starter weights, fixtures, floors and a `packtest`. Uniform priors in shadow for
  tenants that have no labels yet.

**Non-goals**
- Code or expressions in config (§5.12).
- Weight fitting (§12 Q8).
- Cross-tenant linking.
- Changes to `Plan`/`Combine` math, other than the stage-gate fix (slice P1s) and per-tenant rule
  sets.
- Declaring vocabularies at runtime.
- A built-in "delivery" event (dropped in revision 2).

## 3. Relevant context and constraints

This design touches the following code on `main` plus #5 and #7.

- `internal/event`
  - Static redaction schema, v2.
  - Unknown types keep every key after the leak scan.
  - ASN travels in the clear.
- `internal/feature`
  - A monolithic `Extract` over *every* stored event.
  - `nextRescoreAt` hard-codes the windowed types.
  - `firstSeenAt` is the minimum `at`.
- `internal/core`
  - `inputHash` keys features by name.
  - `quantizeAgeFeaturesForHash` and `stageSkip` switch on literal feature names.
  - `maxRiskByScorer` includes shadow rules.
- `internal/model/local` sums weights in sorted-name order. Its `Version()` hashes the `Weights`
  struct.
- `internal/model/fake`: `deterministicProbs` hashes the sorted feature names and values.
- `internal/worker` / `internal/serve`
  - `renderReason` prints flat names.
  - `computeVerdict` and `currentRuleNames()` read one global config.
- `internal/store`
  - `EventsForSubject` loads everything, ordered by `(at, seq)`.
  - `corpus_examples.features` is keyed by name.
  - `subjects.first_seen_at` uses `LEAST`.
- `eval` (#5)
  - `LoadSnapshotCorpus` reads corpus-v1.
  - `hashScoreRequest` keys cassettes by feature names.
  - `run.json` records SHAs.
  - `eval/neighbors.go` evaluates evidence as of the scoring instant.

**Assumptions**
- A1. S5 and S8 haven't shipped, so no production verdicts, corpus rows or vendor cassettes exist
  yet.
- A2. At most about 100 tenants and 64 custom features per tenant. Text is retained for 90 days.
- A3. Producers hash any identifier they want counted, and abusekit re-hashes it anyway.

## 4. Proposed design: overview

```
private config mount: tenants/<t>/profile.yaml + history/NNNN.yaml (append-only, CI-checked)
                     │ compile + validate per tenant (atomic, isolated)
                     ▼
ingest ─▶ vocab.Redact(tenant): leak scan · kinds · re-HMAC · domain allowlist/HMAC · drop undeclared
       ─▶ store (one transaction): event row · event_subjects index rows (≤ 8)
                                   · subject_facts upsert · subject_counters increments · dirty marks
worker (per-tenant fair queue)
       ─▶ feature.Extract(profile), in pass order (§5.7):
            F  facts + counters (O(1))
            N  anchored windows (frozen facts after close)
            A  exact per-feature aggregate queries
            R  row features with per-feature budgets (peak, relative baselines)
            G  Go-pack residue, per-pack budget
            D  derived (ratio, absent indicators)
       ─▶ core.Plan / Combine (math unchanged) ─▶ verdict {…, partial: [features]}
```

| Module | Interface | Deletion test |
| --- | --- | --- |
| `internal/vocab` (new) | `Compile(builtin, decl) (*Vocabulary, error)`; `Redact(*event.Event, Keys) (Stored, error)` | Without it, redaction, kinds, roles and allowlists spread across ingest and packs. |
| `internal/secret` (new) | `Keys` plus HKDF `Derive` (§5.6) | Two adapters: file and cloud secret manager. |
| `internal/facts` (new) | `Apply(tx, Stored)` at ingest; `Read(tenant, subject)` | Onboarding and lifetime values live in one place. |
| `internal/pack` (new) | `Pack` plus a registry. Two Go adapters, `core` and `brand`, both domain-neutral (§5.0). A third adapter, `declared`, compiles any YAML pack: reference packs such as `email` and `card-testing`, and a tenant's `custom` features. | Three adapters, so the seam is real. |
| `internal/pack/custom` (new) | `Compile(tenant, []Def, *Vocabulary) (Pack, error)` | Holds every DSL semantic. |
| `internal/evalengine` (new) | `Aggregate(ctx, Query) (Result, error)`; Postgres and in-memory adapters | Two adapters (store and eval replay), proven equal by conformance tests. |
| `internal/feature` | `Extract(ctx, profile, subject, now) (Result, error)` | Thin orchestrator called by the worker, evaluate and eval. |
| `internal/config` | `LoadProfiles(mount, deps)` | Absorbs `rules.yaml`; reloads atomically per tenant. |

## 5. Proposed design: detail

### 5.0 The domain-neutral binary (revision 6)

**What stays compiled into the binary**, and only this:

1. **The engine.** Ingest and redaction by field kind, keys and pseudonymisation, facts, counters,
   the aggregate engine, the DSL compiler and evaluator, scoring, and the HTTP surface.

   The leak scanners (email, card, IP and phone shapes) are privacy detectors, not domain features.
   They exist to *reject or mask* personal data in any product's events, and they appear on the
   neutrality allowlist with that justification.
2. **Built-in core vocabulary.** After revision 6, "built-in" means types, fields and link kinds
   whose schema is compiled into the binary and needs no pack. There are exactly:
   - **Subject lifecycle:**
     - `subject.created`: `channel`, `identity_kind`, `account_created_at`;
     - `subject.deleted`: `mode`;
     - `subject.class`: `class`.
   - **Payment:** `payment.attempt`: `outcome`, `reason`, `funding`, `amount_minor`, `currency`.
   - **Subscription:** `subscription.changed`: `plan`, `status`, `amount_minor`.
   - **Resource:** `resource.created` / `resource.deleted`: `kind`, and `name`, which carries the
     role `display_name`.
   - **Verdict:** `content.verdict`: `source`, `category`, `score`.
   - **Label:** not an event type. Labels arrive through `POST /v1/labels`, whose vocabulary
     (`benign`/`abusive` plus each rule's labels) stays built-in.
   - **Link kinds:** `card_fingerprint_hash`, `device_hash`, `ip24_hash`, `ua_hash` and `asn`.
3. **Two Go packs, both domain-neutral:**
   - `core@1`. It reads only built-in core types, declared roles (`activity`, `credential`,
     `self`) and link kinds marked `evidence`.
   - `brand@1`, the impersonation matcher described below.

**Everything else is declared.** Declarations live in a YAML pack or in a tenant profile, and they
include:
- `content.sent`: its fields `recipient_domain`, `recipient_is_own_identity`, `recipient_hash`,
  `recipient_count`, `subject_line` and `first_link_host`, its cross-field rule, and its
  masking and raw-storage settings;
- the `email_hash` link kind;
- the `subject.created.email_domain_class` and `resource.*.address_domain` fields;
- the webmail list;
- every `email.*` feature.

The **email reference pack** (`packs/email/`) declares all of these. The wire contract stays
byte-compatible: e2a keeps sending `content.sent`, `links.email_hash` and `email_domain_class`
exactly as today.

**Two generic vocabulary mechanisms make that possible:**
- **Pack extensions of built-in types.** A *pack* (not a tenant) may add fields to a built-in type
  under unprefixed names, for example `email_domain_class` on `subject.created` and
  `address_domain` on `resource.*`. Two enabled packs extending the same name fail with
  `extension_conflict`. Tenant extensions keep the `x_` prefix.
- **A flat `links` map.** `links` is a map of **declared** link kinds, built-in or pack-declared,
  so `links.email_hash` stays a top-level key. Revision 2's `links.custom` is dropped.
  - **Neighbour evidence.** Which kinds count as evidence is set by `evidence: true` on the
    declaration, never hard-coded. The email pack marks `email_hash` as evidence, which
    reproduces today's default of `{email_hash, card_fingerprint_hash, device_hash}` when the
    email pack is enabled.
  - **Canonical serialisation (M1), pinned.**
    - The legacy kinds are written first, in today's `event.Links` field order: `email_hash`,
      `card_fingerprint_hash`, `ip24_hash`, `asn`, `ua_hash`, `device_hash`, each omitted when
      empty.
    - Any other declared kinds follow, sorted by name.
    - An empty map serialises as `{}`.

    This keeps the stored `links` bytes and `body_hash` unchanged for every pre-P-E1 event.
  - **Validation is unchanged for legacy kinds.**
    - `email_hash` keeps its 64-lowercase-hex check. It is declared as a link kind with
      `format: hex64`.
    - A malformed legacy value is still a per-item `bad_links`.
    - An **unknown link key** is still a whole-request `400 bad_request`. Today strict JSON decoding
      produces that, and from P-E1 the tenant's declared-kind set does.
  - **Byte-identity test (`TestWireByteIdentity`).** It replays the whole pre-P-E1 corpus (every
    committed fixture plus the synthetic corpus, and every error-path contract test) through the
    old and new ingest paths. For each event it asserts identical stored `links` and `data` bytes,
    identical `body_hash`, and an identical outcome: accepted, `duplicate`, `conflict`, or the
    exact error code.
- **Derived fields (M2).** Fields that ingest computes, such as `x_<field>__in_<set>` and
  `x_brands`, are stored in a separate `events.derived jsonb` column and never in `data`. They are
  excluded from the `body_hash` canonical form and from the 8 KiB `data` limit. A set or matcher
  change recomputes them by backfill (§5.6 warm-up).
- **`display_name` is opt-in per declared resource kind (L3).** `resource_kinds:
  {agent: {role: other, display_name: true}}`. The implicit legacy declaration sets
  `display_name: true` for every kind, including undeclared ones, because the Go `nameBrandMatch`
  reads every resource name.

**Reference packs are embedded and addressed by content SHA (H4, M5).**
- The binary embeds `packs/**` (`go:embed`) together with a **release manifest**
  (`packs/MANIFEST`) that lists each reference pack's `name@x.y.z` and the SHA-256 of its
  directory.
- The embedded packs are data, never Go. Loading a reference pack never depends on a mount, so a
  missing file can't disable one.
- **A pack counts as a reference pack only if its SHA is in the embedded manifest.** Only those
  packs may use `compat` options.
- Reserved pack names (`core`, `brand`, `email`, `card-testing`, `api-credential-abuse`, `custom`,
  and every manifest name) **can't be shadowed from a tenant mount**. A same-name directory
  in the mount fails with `pack_shadowing`.

**Fail closed on configuration errors (H4).**
- If a tenant is in `config_error` (no valid profile: a cold start with a bad profile, or an
  embedded pack SHA mismatch), `POST /v1/events` for that tenant returns **`503` with
  `Retry-After` for the whole request**, and code `tenant_config_unavailable`.
- Ingest never drops fields, never rejects single items, and never stores events under a partial
  vocabulary. The producer's durable outbox retries.
- **Test (`TestIngestFailsClosedOnConfigError`):** with the tenant's profile invalid, a valid
  batch returns 503 and nothing is stored. Once the profile is fixed, the retried batch is stored
  with bytes identical to a batch that never failed.

**Core neutrality audit.** Every revision-5 core feature was checked:

| Feature | Hidden email assumption | Revision 6 |
| --- | --- | --- |
| `core.burst_ratio_24h_vs_lifetime` | It counted `content.sent` by type name. | Generalised: it counts `resource.created` plus every type with role `activity`. The email pack declares `content.sent` as `activity`, so values are identical. |
| `core.email_domain_class_disposable` (new in revision 2) | It reads `email_domain_class`. | Moved to `email.domain_class_disposable`. |
| `core.linked_*`, `core.fingerprint_seen_on_other_subjects` | The default evidence kinds included `email_hash`. | The evidence set is declared: `evidence: true` on link kinds. The fingerprint feature keeps reading the built-in `card_fingerprint_hash`. |
| `core.subject_age_h`, `upgrade_*`, `declines_*`, `first_funding_prepaid`, `resource_*`, `credential_*`, `verdict_max_24h`, `neighbors_truncated` | None | Unchanged |
| Self-send, webmail and destination logic | Previously Go facts and counters | Now entirely in the email pack, as declared `before_first` fact specs and lifetime share counters over the roles `self`, `recipient` and `destination` |

**Completed audit of non-feature code (M3).** Every item below is fixed in slice P-N0 (the
cleanup that runs before the neutrality CI) or deleted in P-E2:

| Location | Domain knowledge | Fix | Slice |
| --- | --- | --- | --- |
| `internal/worker/reason.go` `renderReason` | Prints email-named features by hand. | Reason is generated generically from the rule's inputs: the top contributing features by `|w·x|`, with their values. `reason_version: 3`. | P-N0 |
| `pkg/abusekit` `Links.EmailHash` (public Go SDK) | Email link kind in a typed struct. The flat map is a **breaking SDK change**. | **Replaced in place** (owner decision, Q32). `pkg/abusekit` on `main` changes `Links` to `map[string]string`. There is no new module path, no second version, and no retirement window. It's pre-GA with no external consumers, so this is simply a breaking change, called out in the release notes and changelog. The wire bytes for the legacy link kinds are unchanged (M1 canonical order). No allowlist entry is needed. | P-N0 |
| `eval/neighbors.go` `defaultNeighborKinds` | Hard-codes `email_hash`. | Uses the profile's declared evidence kinds. | P-N0 |
| `eval/gen` | Generates `content.sent` families in Go. | Becomes a data-driven generator. Families are YAML in each pack (`packs/<p>/gen/*.yaml`) or example profile; the Go generator stays neutral. | P-N0 |
| `internal/feature/store_neighbors.go` `allLinkKinds` | Lists `email_hash`. | Derived from the tenant's declared link kinds. | P-N0 |
| Brand matcher literals `buildIntegrationTokens` and `buildCommunityPhrases` | Word lists in Go. | Moved to `packs/brand/*.yaml`. | P-N0 |
| YAML config: `config/rules.yaml`, `local_weights.yaml`, `webmail.yaml`, `brands.yaml` | Email feature names and lists. | Moved to `examples/tenants/reference/`, `packs/email/` and `packs/brand/`. | P-N0 (moves; the golden stays exact) |
| #7 `--webmail` flag and `ABUSEKIT_WEBMAIL_CONFIG` | An email concept in the CLI and environment. | Deleted in P-E2. For one release the flag and variable are accepted, ignored and logged as deprecated. **Ops migration note:** the hosted compose (S8) removes them in the same release that bumps the email pack pin. | P-E2 |
| #7 `WebmailSet` dependencies in `serve`, `labels` and `worker` | Webmail threaded through the core. | Deleted; the webmail list is pack data. | P-E2 |
| #7 `"content.sent"` matches in `windows.go` (`isWindowedEventType`, sends helpers) | Type name in core code. | Core windowed types derive from `Reads`/`activity`. The sends helpers are deleted with the Go email code. | P-E2 |
| #7 `"agent"` match in `exemptSubjectBrands` (and the `feature.go` comment) | e2a resource-kind name. | Becomes the declared exemption `where: {field: kind, in: [agent]}` in the email pack (§5.5, H1). | P-E2 |

**Roles (field level).**
- `title`: text matched by `brand.title_match`.
- `display_name`: an actor-chosen name matched by `brand.name_match`. The core
  `resource.*.name` field carries it for every resource kind that opts in (L3).
- `self`: bool; a self-directed event.
- `destination`: domain or hash; where an activity lands.
- `recipient`: hash; a single counterparty. Pairs with `recipient_count`.

Roles only let **neutral** code (core and brand) find fields generically. The DSL always names
fields explicitly.

**The brand pack: what remains Go, and why that is neutral.**
- **In Go:** the matcher. It applies NFKC and a confusables skeleton, strips `Cf` characters,
  tokenises on Unicode punctuation and symbols, splits camelCase, and matches on token boundaries
  (including multi-word sequences and case-sensitive entries). It also runs two gate mechanisms,
  the integration-token gate and the community-phrase gate. All of this is string processing over
  *any* field with role `title` or `display_name`, and it knows nothing about mail or any product.
- **Moved from Go literals to data:** the brand list (`packs/brand/brands.yaml`, plus a tenant's
  private `extra`); the integration tokens (`packs/brand/integration_tokens.yaml`, formerly
  `buildIntegrationTokens`); and the community phrases (`packs/brand/community_phrases.yaml`,
  formerly `buildCommunityPhrases`).
- **Exposed** as features `brand.name_match`, `brand.name_has_at` and `brand.title_match`, and as
  one generic DSL op, **`brand_match`** (§5.5), which declared packs may use.

**Neutrality CI.** Switched on at the end of P-E2 (§9), after the P-N0 cleanup. It then gates
every slice.
1. `TestCoreIsDomainNeutral` (M4).
   - **Scope:** it walks the Go AST of **every** Go file in the module, including Go under
     `packs/` and `_test.go` files. Test files are scanned too, so domain knowledge can't hide in
     test helpers. Data files (YAML, JSON, JSONL, text) are not scanned.
   - **Tokenising:** each identifier and string literal is split on camelCase and snake_case
     boundaries and on non-alphanumerics, then lowercased. Every token is compared against
     `internal/neutrality/denylist.txt`: `email`, `webmail`, `recipient`, `subject`+`line`,
     `smtp`, `mailbox`, `inbox`, `e2a`, `agent`.
   - **`agent`** is denied except as the second token of `user agent` (`UserAgent`, `user_agent`,
     `ua_hash` is unaffected). The rule is explicit in the denylist syntax: `agent !after user`.
   - **Allowlist:** `allowlist.txt` is keyed by `(file, identifier)`, one justified entry per line,
     with an optional expiry date. The leak scanner gets exactly three entries, all in
     `internal/vocab/leakscan.go`: `emailRe`, `looksLikeEmail` and `maskEmails`.
     `emailMaskExemptKey` is deleted. Which field is masked instead of rejected is now the pack's
     `text` field declaration.
2. `TestNonEmailProfileEndToEnd`. The binary loads the card-testing reference pack
   (`examples/tenants/tallyport`) with **no email pack**. It ingests that pack's fixtures through
   HTTP and scores them. It asserts that no `email.*` feature is registered, that `content.sent` is
   an undeclared type for that tenant, and that verdicts match the pack's golden replay.
3. `TestGoRegistryLint`. Every Go `FeatureDef` may read only built-in core types or roles
   (`Reads`). Any type name outside the built-in list fails.
4. **Runtime enforcement of `Reads`.** A Go pack's `Extract` receives a **filtered event view**
   that contains only events whose type is in its `Reads`, directly or by role. A Go pack
   therefore can't observe a pack-declared type even through a generic loop over events.

### 5.1 Packs

```go
type Pack interface {
    ID() ID                 // {Name: "email", Version: 1}
    Requires() []string     // e.g. email@1 → [core]
    Features() []FeatureDef // in registry order
    Extract(ctx context.Context, in Input) (Output, error) // pure; passes in §5.7
}

type FeatureDef struct {
    Name          string   // "email.sends_10m_max"; grammar §5.3
    Order         int      // local-scorer summation order (§5.2), fixed forever
    Class         Class    // F | N | A | R | G | D (§5.7)
    HashQuantum   float64  // input-hash bucket (replaces the name switch in core)
    Bound         float64  // normalisation ceiling for uniform priors (§5.9); never a value cap
    PriorSign     int8     // +1 / -1
    Reads         []string // types/roles read: dispatch, rescore scheduling, dirty marks
    RequiresPacks []string // e.g. email.subject_brand_match → [brand]
    SubjectKinds  []string // default [account]
}

type Output struct {
    Values  map[string]float64
    Rescore []time.Time
    Partial []string // features whose own budget was hit (§5.7)
}
```

**Enablement**
- A profile lists its packs: `packs: [core@1, email@1, brand@1]`.
- `core` is always on. Other packs are opt-in.
- A pack's major version pins its semantics.
- Unmet `Requires` fails with `pack_requires`.
- A feature whose `RequiresPacks` or `SubjectKinds` don't match isn't registered for that tenant
  or kind.
- Each pack owns its namespace, so feature names can't collide.

**Pack contents.** The *Class* column says where each feature's inputs come from (§5.7).

| New name (old name) | Class | Source |
| --- | --- | --- |
| `core.subject_age_h` | F | `start` (§5.7) |
| `core.upgrade_delay_min`, `core.upgraded` | F | `subject_facts.first_paid_upgrade_at` |
| `core.declines_before_first_success` | F | `subject_facts.declines_before_first_success` |
| `core.first_funding_prepaid` | F | `subject_facts.first_success_funding` |
| `core.fingerprint_seen_on_other_subjects`, `core.linked_deleted_n`, `core.linked_labelled_abusive_n`, `core.neighbors_truncated` | F | neighbour evidence, built-in link kinds only (§5.5) |
| `core.resource_velocity_1h`, `core.credential_velocity_1h` (`key_velocity_1h`) | A | exact 1 h counts |
| `core.resource_total`, `core.credential_total` (`key_total`) | F | `subject_counters` (lifetime) |
| `core.burst_ratio_24h_vs_lifetime` | A + F | 24 h count (A) ÷ lifetime counters, over `resource.created` + `activity` types |
| `email.*`: all nine #7 email features, plus `email.domain_class_disposable` | per DSL op | **Declared** in `packs/email/pack.yaml`, not Go (§5.5 parity table). Until the parity slice deletes it, the #7 Go code is transitional. |
| `brand.name_match`, `brand.name_has_at` | F | `subject_facts.name_brands`, `name_has_at`, matched at ingest over `display_name` fields |
| `brand.title_match` (new) | A | distinct brand ids across non-self `title` fields in 1 h |
| `core.verdict_max_24h` (new) | A | not part of e2a's rule |

For the rows above, the code changes from scanning every event to reading facts, counters and
exact aggregates. The arithmetic doesn't change: it operates on the same inputs and sums integers,
which float64 represents exactly below 2⁵³. The golden replay proves this.

**Bound** is only a normalisation ceiling for uniform priors, which clamp to it. It never caps a
value.

| Feature | Bound |
| --- | --- |
| `core.resource_velocity_1h`, `core.credential_velocity_1h` | 50 |
| `core.resource_total`, `core.credential_total` | 1,000 |
| `core.linked_labelled_abusive_n` | 10 |
| `core.subject_age_h` | 24 (already clamped) |
| `core.upgrade_delay_min` | 1,440 (already clamped) |

Revision 2's 1,000 value cap on totals is dropped, because totals now come from counters.

### 5.2 The one-time rename, bit-exact

The rename happens once, in P1. No alias table outlives that slice.

**Summation order doesn't change.** The local scorer used to sum in sorted feature-name order,
which a rename would reshuffle. It now sums in `FeatureDef.Order`:
- legacy features keep the index of their old flat name in the old sorted order (0–24);
- new pack features take indices from 100 up;
- custom features follow, in sorted-name order.

The additions happen in exactly the same sequence as before, so risks and tiers are bit-exact.
Revision 2's 1e-12 tolerance and cut-point proximity check are deleted.

**Every consumer of feature names, and its test**

| Consumer | Change | Test |
| --- | --- | --- |
| `core.stageSkip` literal `features["subject_age_h"]` | Reads `core.subject_age_h`. `Rule.Stage` **keys** (`max_subject_age_h`, `min_subject_age_h`) are stage names and stay unchanged. | Table test with a namespaced vector. A grep test fails on flat literals in `core`. |
| `core.quantizeAgeFeaturesForHash` | Deleted. Buckets come from `FeatureDef.HashQuantum`, plus `hash_quantum` for custom features (§5.5), passed in `core.Vector`. | The hash-drift tests are re-run. |
| `core.inputHash` | Changes once. | The golden records both values; stable afterwards. |
| `local.Scorer` summation and `Version()` | Order follows `FeatureDef.Order`. `Version()` changes once, and the weights file's `version:` becomes `v2`. | Golden risk bits are identical; `Version()` is recorded before and after. |
| `fake.deterministicProbs` (hashes sorted feature names) | Output changes once. P1 re-baselines every fake-based expectation: contract tests, eval tests and fake cassettes. | The committed re-baseline is stable afterwards. |
| Eval cassette key (`hashScoreRequest`) | Changes once. Cassettes (fake and local only, per A1) are re-recorded, and the header gains `feature_key_space: ns-v1`. | A cassette with a mismatched key space fails loudly. |
| `eval.LoadSnapshotCorpus` | The schema bumps to `corpus-v2` (namespaced keys plus `feature_key_space`). A v1 row with a flat name fails with `feature_renamed`, which names the new feature. Nothing is translated silently. | Loader tests. |
| `run.json` SHAs | `rule_sha` and `weights_sha` change once. The manifest gains `feature_key_space`. | The golden compares metrics, not SHAs. |
| `worker.renderReason` / stored reasons | Namespaced names, plus `reason_version: 2`. Old rows are left as they are. | Snapshot test. |
| `corpus_examples.features` | New column `feature_key_space`, defaulting to `flat-v0`. A migration rewrites keys to `ns-v1` (production has no rows, per A1). The loader refuses `flat-v0`. | Migration test on a seeded DB. |
| `feature.Names`, `Map`, `FeatureSet`, `rules.yaml`, weights, mutation/ablation/golden-sign tests, fixtures README | Mechanical rename. | Full suite plus the grep test. |
| `abusekit score --jsonl` | Flat names fail with `feature_renamed`. | CLI contract test. |

**Enforcement.** P1 lands before any S5 or S8 PR, and the v0 plan marks S5 and S8 "blocked on P1".
`TestNoProductionBeforeRename` guards both sides:
- **S5:** it fails if `internal/model/{gemini,jev,laya}` exists while `feature.KeySpace != "ns-v1"`.
- **S8:** it asserts that `abusekit serve` refuses to start with `ABUSEKIT_ENV=production` unless
  `feature.KeySpace == "ns-v1"`. The hosted deploy (S8) always sets that variable, so a pre-rename
  binary can't be deployed.

### 5.3 Name grammar

- **Features:** `^[a-z][a-z0-9]*\.[a-z][a-z0-9_]{0,55}$`.
  - Reserved namespaces: `core`, `email`, `brand`, `custom`, and any future pack.
  - Tenants define only `custom.*`, scoped to the tenant.
  - The suffix `__absent` is reserved for derived indicators (§5.5).
- **Types:** `^[a-z_.]+$`, at most 64 bytes, with a dot.
  - Reserved prefixes: `subject.`, `payment.`, `subscription.`, `resource.`, `content.`,
    `abusekit.`.
- **Extension fields:** `x_<name>`.
- **Stored undeclared names:** see §5.6.

### 5.4 Product-declared vocabulary

```yaml
vocabulary:
  version: 4
  subject_kinds:
    account: {}
    api_key: {parent: account}
    card:    {}
  resource_kinds:
    key:   {role: credential, aliases: [keys, api_key, api_keys, api-key, apikey, "api key"]}
    agent: {role: other}
  link_kinds:
    phone_hash:     {evidence: true}
    oauth_sub_hash: {evidence: true}
  types:
    invite.sent:
      role: activity
      fields:
        invitee_hash: {kind: hash, join_domain: member}
        target_class: {kind: enum, values: [own_community, other_community]}
        preview:      {kind: text, role: title, max_len: 200}   # skeleton-only by default
        link_host:    {kind: domain}                            # reduce: etld1 by default
  extend:
    resource.created:
      x_visibility: {kind: enum, values: [public, private]}
```

**Roles.** Only these roles ship:
- resource kinds: `credential` and `other` (an undeclared kind counts as `other`);
- event type: `activity`;
- fields: `title`, `display_name`, `self`, `destination` and `recipient` (§5.0).

`brand.title_match` counts distinct brand ids in the `title` fields of non-self activity events
over the trailing hour, capped at 3.
- The brand ids are matched at ingest, on the value before skeleton reduction, using #7's matcher.
- They are stored as the derived field `x_brands`.

**Subject kinds.** Two optional wire fields are added: `subject_kind` (default `account`) and
`also` (a list of up to 3 `{kind, id}` entries).
- The event row is stored once.
- `event_subjects` gets one row for the primary subject and one per `also` entry, at most 4.
- It also gets one `via_parent` row for each of those subjects that has a declared parent.
  Parents can't have parents, so an event has at most 8 index rows.
- Each indexed subject is marked dirty. The parent rows let account-level features see events from
  child keys.
- Non-account ids and `also` ids are pseudonymised (§5.6).
- Subjects are keyed `(tenant, kind, id)`.
- `GET /v1/subjects/{subject}?kind=` and `evaluate` derive the key from the path id in the same
  way.
- Rules declare `applies_to`.

**Link kinds.** `links` is a flat map of declared kinds (§5.0), holding at most 8 non-built-in
kinds, and every value is re-HMACed. Kinds declared with `evidence: true` feed the `core.linked_*`
features when their pack declares them as core evidence (the email pack does this for
`email_hash`). They also feed `neighbours`. A tenant's own declared kinds feed only `neighbours`,
so core evidence is never double-fed.

**Legacy default.** A profile without `resource_kinds` gets `key: credential`, with #7's aliases.

### 5.5 Declarative custom features

```yaml
features:
  - name: custom.public_links_1h
    version: 1
    description: public share links created in the last hour
    subject_kinds: [account]
    count:
      type: share.link_created
      where: {field: visibility, eq: public}
      sum: {field: size_class, default: 1, cap_each: 10}   # optional; integer field only
    window: 1h                 # or first: <dur> | lifetime | before_first: {type, where}
    transform: {log1p: true, cap: 50}
    hash_quantum: 0            # optional; defaults below
    prior_sign: "+"
```

**Windows**

| Window | Range |
| --- | --- |
| `window: <dur>` | `(now − dur, now]`, for any duration from 1 minute to 30 days |
| `first: <dur>` | `[start, start + dur)`, class N |
| `lifetime` | every ingested event with `at ≤ now`, served from counters (class F). Only valid on `count`/`sum`. |
| `before_first: {type, where}` | `(−∞, t_B]`, where `t_B` is the first matching event at or before now, or `(−∞, now]` if there is none. Served from facts, only valid on `count`. The end is inclusive, as in #7. |

**Predicates.** The set is closed and has no regex:
- `eq`, `ne`, `in`, `not_in` (at most 256 values; enum values are checked at load);
- `in_set`, `suffix_in_set` (set files of at most 100k entries, leak-scanned at profile load).
  - **On a `domain` field**, both are evaluated **at ingest** against the cleartext value, before
    any HMAC. The result goes into a derived bool `x_<field>__in_<set>`, which the predicate then
    reads.
  - Changing a set file bumps the vocabulary version and triggers a backfill. Features that use
    the set stay cold until the backfill completes.
  - `suffix_in_set` on any other kind is rejected at load.
- **Literals on pseudonymised fields.** `eq`/`in` literals and set files on `hash` fields are
  HMACed at load, the same way ingest hashes values. During a rotation they are hashed under
  **both** keys, and a predicate matches either one.
- `gt`, `gte`, `lt`, `lte`, `exists`;
- `all`, `any`, `not`, with nesting depth at most 2 and at most 8 leaves.

A leaf over an absent or mistyped field is false. Every predicate is a parameterised query shape,
never user SQL, so all of them push down to the aggregate engine. `text` fields can't be used in
predicates, `distinct`, `group_by` or `on`.

**Operations**

| Op | Definition at `now` | Class and cost |
| --- | --- | --- |
| `count` | Matching events in the window. With `sum`, the sum of an integer field: `default` if absent, clamped to `cap_each`. | A; one aggregate |
| `distinct` | `{field}`: the number of distinct values. | A; `count(DISTINCT)` |
| `share` | `{where, match, sum?}`: numerator over `where ∧ match` ÷ denominator over `where`. With `sum`, **both** numerator and denominator sum the field. An empty denominator gives `if_empty`. | A; one aggregate |
| `peak` | `{size, sum?}`: the maximum of `agg(E ∩ (t − size, t])`, over `t` at the instants of matching events inside the outer window. `agg` is a count, or with `sum`, a sum. Sub-windows are clipped to the outer window. | R; exact under a saturation limit (§5.7) |
| `time_between` | `{from: {type, where, anchor: first\|last}, to: {type, where}, until_now, if_absent}`. `t_A` is the first (or last) matching `from` with `at ≤ now`. `t_B` is the first matching `to` with `t_A ≤ t_B ≤ now`. The value is minutes from `t_A` to `t_B`, or `now − t_A` when `until_now` is set and there is no `t_B`. Absence semantics are below. | A; indexed first/last-match queries |
| `sequence` | `{a, b, within ≤ 24h, on?: {a: f, b: g}}`: the number of `b` events in the window with an `a` event where `t_a ∈ (t_b − within, t_b]` and, if `on` is set, `a.f == b.g`. Both `on` fields must be `hash` fields with the same `join_domain`. | A; aggregate with a correlated existence test |
| `group_by` | A modifier on `count`, `distinct` or `sum`: `{field, reduce: max \| {count_gte: k}, max_groups}`. It groups by `field`, applies the op per group, then reduces: `max` takes the largest group value, `count_gte` counts groups at or above `k`. **Exact**: the aggregate engine groups every matching event, so there is no first-come admission for decoys to exploit. `max_groups` (at most 1,000) bounds only the in-memory adapter. Past it, the in-memory adapter uses space-saving (Metwally) with `k = max_groups`. Space-saving over-estimates tracked counts and loses evicted groups. `reduce: max` with a positive sign stays one-sided upward and is flagged `partial` only. `count_gte` can undercount groups whose true count is at or above `k` but that were evicted. A negative `prior_sign` or weight inverts the direction. Those combinations are flagged `partial` **and** `degraded`. Postgres is always exact. | A; `GROUP BY` |
| `ratio` | `{num, den, if_empty}` over the **pre-transform** values of two non-ratio custom features. Depth 1: no cycles, no ratio of ratios. **Partial propagation:** a `partial` input makes the ratio `partial`. `den` may not be a feature that can go partial (`relative_to_history`, `peak`, `group_by`, `neighbours` or any class N `first:` feature); the load fails with `ratio_den_partial_capable`. A partial `num` inherits its own `degraded` status. If a partial-capable `num` meets a **negative** ratio sign (the ratio's `prior_sign`, or its weight in any weights file), an upward error in `num` would lower risk. The ratio is then flagged `partial` + `degraded` whenever `num` is partial, the same treatment space-saving gets. | D; O(1) |
| `neighbours` | `{via: [declared link kinds], where: {deleted: permanent} \| {labelled: abusive} \| {created_within: <dur>} \| {}}`: the number of distinct other same-tenant, same-kind subjects that share a `via` key and meet the condition. **As of `now`**: links with `first_seen ≤ now`, subjects created ≤ now, deletions and labels ≤ now. `created_within` is relative to `now`. This matches `evidenceAsOf` in `eval/neighbors.go`. **`where` is applied before any limit.** The query returns distinct matching subjects up to `K = ⌈T⁻¹(cap)⌉ + 1` (§5.7, saturation), so the count is exact up to saturation. The per-key fan-in cap no longer truncates the counted set. If the query's examined-row budget (default 50,000) runs out before it reaches `K` or finishes, the value is `partial` **and** `degraded`, because an undercount could lower risk. | F/A; one indexed query per `via` set; at most 4 per tenant |

**Absence semantics** (`time_between`, `sequence`)
- `if_absent` is a mandatory number.
- The loader also derives a 0/1 indicator, `<name>__absent`, and `absent_sign` is mandatory.
- This matters for abandonment. Without the indicator, an account that signs up and never uses the
  product would read as the most benign value on the duration scale. With it, a rule can weight
  abandonment separately.
- Uniform priors use `absent_sign` as the indicator's sign.
- Durations should use `log1p`, for example `transform: {log1p: true, cap: 9.3}` for up to a week.
  All examples do.

**Neighbour evidence stays separate**
- `core.linked_*` read only link kinds that a *pack* declares as core evidence (`evidence: true`), plus the built-in kinds the tenant's `feature.Config` admits. `core.fingerprint_*` reads the built-in `card_fingerprint_hash`.
- Kinds a *tenant* declares feed only `neighbours`, so they never double-feed `core.linked_deleted_n`.
- **Dirty-mark propagation** covers built-in *and* declared evidence kinds. When a subject gains a
  link key, is permanently deleted, or is labelled, every subject sharing any evidence key with it
  is marked dirty.
  - The first 50 per key are marked in the ingest transaction.
  - The rest are marked by a background job that pages through them. It runs under a per-tenant
    rate budget, and a metric counts the marks still pending.
  - A subject whose mark is still pending picks up the change on its next event or timer rescore.
    Its evidence is always read as of `now` and is never cached, so a late rescore sees the change
    in full.
- **Legacy `core.linked_*`** keep their frozen caps (50 per key, 200 in total) and
  `core.neighbors_truncated`, for bit-exactness. When a cap is hit, the verdict now also carries
  `partial` and `degraded`, because the undercount lowers a positively weighted value.

**`relative_to_history`** applies to `count`, `distinct` and `peak`:

```
cur   = op over its window at now                        (class A, or R for peak: exact)
E_b   = { matching e : now − lookback < e.at ≤ now − exclude_recent }
base  = baseline_op over E_b      (default: same op; count/distinct over W → max over sliding
        windows (t − W, t], t ∈ instants of E_b, clipped to E_b; peak → peak over E_b)
v1    = min( cur / max(base, 1), ratio_cap )             ratio_cap defaults to transform.cap
d     = clamp( 1 − (age_days − full_until)/(zero_at − full_until), floor, 1 )   if age_decay
value = transform( v1 · d )
```

- `base` is class R, with its own row budget.
- If that budget is hit, `base` is computed over the newest rows of `E_b`. That can only
  under-estimate the maximum, so `v1` can only rise. The value is flagged `partial`.
- These features must have `prior_sign: +`. A negative weight on one is rejected
  (`baseline_weight_negative`), so truncation can't lower risk. All four of e2a's history-relative
  features have positive weights.
- `age_decay` defaults to `{full_until: 3d, zero_at: 30d, floor: 0.2}`.
- `baseline: {peak: {size: 10m}}` overrides the baseline op.

**Generic primitives added in revisions 6 and 7, so the email pack needs no Go.** Each one is
domain-neutral and usable by any pack.

| Primitive | Semantics |
| --- | --- |
| **Baseline override** `baseline: {op: count \| sum \| distinct \| peak, size?, where?, sum?: {field, default, cap_each}}` | The baseline uses its own op, sub-window width, predicate and summed field, independent of `cur`. **Only monotone ops are allowed** (count, sum, distinct, peak), so a truncated baseline can only under-estimate (§5.7). `lookback` and `exclude_recent` stay on `relative_to_history`. |
| **`distinct` with `on_missing`** `{field, on_missing: ignore \| count \| {sum: {field, default, cap_each}}}` | Rows without `field` are ignored (the default), counted as one unit each, or contribute a summed field. The value is `count(DISTINCT field) + fallback`. |
| **Compat options** (M5): `compat: [<option>, …]` | Explicit, versioned reproductions of legacy quirks. **A closed enum defined in the engine.** Each option has its own conformance test, and adding a new option needs a design change. Options are usable only by manifest-listed reference packs. The v0 options are below. |
| **`share` over `lifetime`** | Numerator and denominator come from two counter specs, with the `(now, +∞)` subtraction (unless `include_future`). `if_empty` applies as usual. |
| **`brand_match`** (H1). Signature: `{role: title \| display_name, window, exclude_self, exempt: {type, where, require_token: integration, match_variant: title \| display_name, live_unless: {type, same_field}}, exclude_brands_of: display_name, cap}` | Counts distinct brand ids matched in `role` fields, provided by the brand pack. Detailed below. |
| **Standalone `age_decay`** (H1, L1) | Without `relative_to_history`, `value = T(min(raw, T⁻¹(cap)) · d)`. With the identity transform this is `min(raw, cap) · d`. `d` is defined exactly below. |
| **Cross-field constraint** (vocabulary) `constraints: [{if_present: f, then: {field: g, lte: 1}}]` | Validated at redaction. A violation is rejected with `redaction_failed`. |
| **Pack rescore mode** `rescore: legacy_v0` (H5, a compat option) | Replaces the pack's rescore candidates with Go's `nextRescoreAt` rule, applied to the pack's windowed types. See below. |

**The `compat` enum, v0.** It lists every legacy quirk the email pack needs:
- `window_end_closed`: an anchored window becomes `[start, start + A]`.
- `include_future`: stop excluding `at > now` for this feature.
- `before_first_include_future` (H2): `before_first` counts, and its boundary, include events with
  `at > now`.
- `rescore_legacy_v0` (H5): the rescore rule described under "Rescore" below.

**`brand_match` in detail (H1).**
- **Matcher variant per role**, pinned by the engine:
  - `title`: community gate on, integration gate off. This is #7's
    `MatchedBrandNamesForSubject`.
  - `display_name`: integration gate on, community gate off. This is #7's `MatchedBrandNames`.
- **Input.** Matching runs on the **stored** value, after masking and truncation, at ingest. It
  writes `x_brands` into `derived`. A backfill that re-runs the matcher therefore sees exactly
  what the scorer saw.
- **Exemption.** `exempt` computes the set of exempt brands **at score time** from the
  `subject_display_names` fact table. That table holds one row per distinct normalised
  resource name, with the resource kind, created and deleted counts, the brand ids under each
  matcher variant, and whether the name has an integration token. Ingest maintains it, so a
  `resource.deleted` is reflected immediately and nothing needs recomputing.
- **Exemption rule.** A brand is exempt if all of these hold:
  - it appears under `match_variant` in the name of a resource matching `exempt.type` and
    `exempt.where`;
  - that name satisfies `require_token`;
  - the name is **live**: no row of `live_unless.type` shares its normalised `same_field`.
- **`exclude_brands_of: display_name`** removes brands matched under the `display_name` variant in
  any resource name, live or not. This reproduces #7's `namedBrandNames`.
- **Counter-example (H1).** A live `kind: key` resource named "Stripe API Key", and a non-self
  subject "Your Stripe invoice":
  - Under the `display_name` variant the name matches nothing, because "API" trips the
    integration gate. So `exclude_brands_of` removes nothing.
  - The exemption's `where: {field: kind, in: [agent]}` excludes the key.
  - So `Stripe` counts. Go gives `1·d`, and the declared feature gives `min(1, 3)·d = 1·d`.
  - A committed parity fixture covers this case.
- **Cost.** The name table is capped at 10,000 distinct names per subject. Beyond that the feature
  is `partial` + `degraded`.

**`age_decay`, exactly (L1).** Let `D = now.Sub(start)` be a Go `time.Duration`. Then:
- `ageDays = D.Hours() / 24`, using Go's `Duration.Hours()`.
- `v = 1 - (ageDays - full_until_days) / (zero_at_days - full_until_days)`, evaluated in that
  order. The denominator is computed once, at compile time, as a float64: 27 for the defaults.
- `d = floor` if `v < floor`; `d = 1` if `v > 1`; otherwise `d = v`.
- The multiplication by `d` is the last operation before the transform, or the value itself when
  there is no transform.

This is #7's `ageDecayFactor` expression tree, including the constants `3` and `27`.

**Rescore (H5): the choice is a pack-level `rescore: legacy_v0` mode, not a list of deviations.**

For the email pack, the rescore candidate set is defined to be exactly Go's `nextRescoreAt`
restricted to the pack's windowed type (`content.sent`):
- the 1 h and 24 h window exits of **every** in-window `content.sent` event, **self-sends
  included**, as in Go;
- the first-day cutover `start + 24h`, while it lies in the future;
- the earliest future-dated `content.sent`;
- all coalesced to fixed 5-minute buckets.

It does **not** add `peak` sub-window exits, baseline-boundary exits, or window-proportional
buckets. The core Go pack contributes the same rule for `resource.created`. The union's minimum is
therefore Go's `NextRescoreAt` exactly, and the golden asserts it bit for bit. Non-reference packs
use the normal rescore rules of §5.5.

**Parity: how every #7 email feature is declared.** All of it lives in `packs/email/pack.yaml`,
over `content.sent`. Every feature declares **`hash_quantum: 0`** (H5), so input hashes equal
Go's.

Two shorthands used in the table:
- **`notself`** = `{not: {field: recipient_is_own_identity, eq: true}}`. An absent field counts
  as not-self, as in Go's `isSelfSend`.
- **`B10`** = the shared baseline (H3): `baseline: {op: peak, size: 10m, where: notself, sum:
  {field: recipient_count, default: 1, cap_each: 300}}`, with `relative_to_history: {lookback:
  30d, exclude_recent: 24h, ratio_cap: 300, age_decay: {}}`.

| Feature | Declaration |
| --- | --- |
| `email.sends_10m_max` | `peak: {size: 10m, sum: {field: recipient_count, default: 1, cap_each: 300}, where: notself}`, `window: 24h`; `B10`; `cap: 300` |
| `email.sends_1h` | `count: {sum: {field: recipient_count, default: 1, cap_each: 300}, where: notself}`, `window: 1h`; `B10`; `cap: 300` |
| `email.webmail_sends_1h` | As `sends_1h`, with `where: {all: [notself, {field: recipient_domain, in_set: webmail}]}`. **The baseline is `B10` unchanged:** all non-self sends, not only webmail. |
| `email.distinct_recipients_1h` | `distinct: {field: recipient_hash, on_missing: {sum: {field: recipient_count, default: 1, cap_each: 300}}, where: notself}`, `window: 1h`; `B10`; `cap: 300` |
| `email.sends_first_day` | `count: {sum: {field: recipient_count, default: 1, cap_each: 300}, where: notself}`, `first: 24h`; `compat: [window_end_closed]`; `cap: 1000000` |
| `email.first_day_distinct_domains` | `distinct: {field: recipient_domain}`, `first: 24h`; `log1p: {anchored_at: 10}`; `compat: [window_end_closed, include_future]`; **`cap: 1000`**, which can never be reached (it would need about e^240 domains) (L2) |
| `email.webmail_recipient_share` | `share` over `lifetime`, `sum: {field: recipient_count, default: 1, cap_each: 300}`, `where: notself`, `match: {field: recipient_domain, in_set: webmail}`; **`if_empty: 0`** (L2); `cap: 1` |
| `email.self_send_before_external` (H2) | `count: {where: {field: recipient_is_own_identity, eq: true}}`, `before_first: {type: content.sent, where: {field: recipient_is_own_identity, eq: false}}` (so an absent field is **not** external, as in Go); `compat: [before_first_include_future]`; `cap: 2` |
| `email.subject_brand_match` (H1) | `brand_match: {role: title, window: 1h, exclude_self: true, exempt: {type: resource.created, where: {field: kind, in: [agent]}, require_token: integration, match_variant: title, live_unless: {type: resource.deleted, same_field: name}}, exclude_brands_of: display_name, cap: 3}`; standalone `age_decay: {}` |

The pack also declares `rescore: legacy_v0` (H5).

**Wire constants and edge conventions.**
- `cap_each: 300` and `default: 1` reproduce `recipientCountOf`: an invalid or non-positive count
  reads as 1, and each value is capped at 300.
- `normalizeToken` (lowercase, then trim) matches the `domain` kind's normalisation for ASCII
  domains.
- **IDNA edge case (L2).** The `domain` kind maps a Unicode domain to its punycode form, which
  the legacy code did not. A Unicode spelling and its punycode twin would count as two domains in
  Go but one in the pack. No committed fixture contains an IDN. This is a documented
  production-only divergence, surfaced by the P-E1 shadow metric (below).
- `log1p: {anchored_at: 10}` computes `s = 10/math.Log1p(10)` exactly as the Go constant does.
- Parity for each feature is **bit-for-bit on the golden replay** (slice P-E2). Any mismatch
  blocks deletion of the Go code.

**Features that stay Go, all neutral:**
- `core.subject_age_h`, which reads `start`;
- `core.linked_*` and `core.fingerprint_*`, which use the evidence semantics;
- `core.burst_ratio_24h_vs_lifetime`, a frozen core quirk that counts future-dated events;
- the `brand.*` features.

**Transform.** `cap` is mandatory: finite, greater than 0, and at most 1 for `share`. `log1p` is
optional and applied before the cap:
- `true` gives `ln(1+v)`;
- `{scale: s}` gives `s·ln(1+v)`;
- `{anchored_at: n}` sets `s = n/ln(1+n)`.

The result lies in `[0, cap]`, and the feature's `Bound` is `cap`.

**Semantics**
- **Pure.** A feature is a function of the stored events, the facts and counters, and `now`.
  - "First" and ties resolve by `(at, producer, id)`.
  - P0 switches the scoring order from `(at, seq)` to that key before capturing the golden.
- **Half-open windows.** `(now − W, now]` and `[start, start + W)`, and `peak` clips its
  sub-windows. `before_first` is the one intentionally inclusive end.
- **Future events.** Events with `at > now` are excluded from custom features and schedule a
  rescore at their `at`.
  - Frozen legacy exception: `core.resource_total`, `core.credential_total` and the
    `burst_ratio` denominator count future-dated events. Counters include them naturally.
  - Frozen legacy exception: `email.first_day_distinct_domains` has an inclusive end.
  - Harmonising these is `@2` work (§12 Q5).
- **Counter exactness.** Custom lifetime counters and the built-in webmail-share counters exclude
  events with `at > now`. The built-in counters match the legacy behaviour, where
  `webmailRecipientShare` excludes future-dated events. Backfill-scope keys are exempt from the
  skew check, so future-dated events aren't limited to +24 h. The class A engine therefore
  subtracts them exactly with an indexed query over `(now, +∞)`.
  - The legacy total counters (`resource_total`, `credential_total`, the `burst_ratio`
    denominator) do not subtract anything, matching their frozen behaviour.
  - Counter `sum` fields must be integers, so the order of addition can't matter.
- **`hash_quantum`.** This lets `SkipInputUnchanged` skip unchanged inputs. Defaults:
  - 60 minutes (pre-transform) for `time_between` with `until_now`;
  - `0.01 × cap` (post-transform) for any feature with `age_decay`, whose value drifts with age on
    every tick;
  - 0 otherwise.
- **Rescore candidates.** A feature proposes:
  - the exit of its oldest in-window match;
  - the end of its anchored window;
  - the exit of the peak sub-window;
  - baseline boundary crossings;
  - future-dated matches.

  §5.8 filters and coalesces them.

### 5.6 Redaction, pseudonymisation, keys, vocabulary history

Ingest consults the tenant **vocabulary**, never rules. What it stores is **pseudonymised**: anyone
holding a key can recompute a hash from a candidate value. Retention and erasure therefore apply to
hashed values too.

**Field kinds**

| Kind | Stored as | Rejected when |
| --- | --- | --- |
| `text` | NFKC-folded. Email, card, IP and phone shapes are **masked** as `@`, `#card`, `#ip`, `#phone`. Truncated at `max_len` (at most 500). Custom text defaults to `store: skeleton`. `store: raw` and **`store: raw+skeleton`** (M2) are opt-in, for text-accepting scorers only. The email pack declares `content.sent.subject_line` as `store: raw+skeleton`, which is today's behaviour: raw plus `subject_line_skeleton`. From P3a it gets the same masking: email (already in #7), card, IP and phone. | Control characters or invalid UTF-8 |
| `number` | **Trusted as declared.** `max` is **required**; `min` and `integer` are optional. There's no Luhn check on numbers: the field is declared and reviewed, and Luhn only falsely rejects amounts and counts. | Out of range, non-finite, or declared without `max` (a load error) |
| `bool` | As is. | Not a bool |
| `enum` | A declared value: at most 64 values, each matching `[a-z0-9_.-]{1,64}`. Leak-scanned at profile load. | Undeclared value |
| `hash` | **Re-HMACed:** `hk<keyid>:` + hex(HMAC-SHA256(k, input))[:32], with `input = lp(type) ‖ lp(field) ‖ lp(value)`. If the field declares `join_domain`, `input = lp("join:" ‖ join_domain) ‖ lp(value)`. `lp` is a u32 length prefix. | Doesn't match `^[A-Za-z0-9_:+/=-]{8,128}$` |
| `domain` | Lower-cased and IDNA-encoded to ASCII. Must end in a public suffix from the pinned PSL snapshot, or in an RFC 6761 special-use name. **Defaults to `reduce: etld1`**; `none` opts out. Stored **in cleartext only if allowlisted**: the email pack's webmail list, the disposable list, or the vocabulary's `popular_domains` set file (top-N). Anything else is stored as `hd<keyid>:` + HMAC. | IP literals, all-numeric labels, unknown suffixes, or an `@` |
| `timestamp` | RFC 3339. | Anything else |

**Email-pack fields in P3b** (`RedactionSchemaVersion` becomes 3). These fields' schema lives in the
email pack from P-E1. Until then it sits in the transitional built-in schema; the handling is
identical either way.

| Field | Reduction | Stored as | Feature impact |
| --- | --- | --- | --- |
| `content.sent.recipient_domain` | **`none`**: `etld1` would merge `target1.example.test` and `target2.example.test`, which would change `email.first_day_distinct_domains` | Cleartext if allowlisted, else HMAC of the normalised value | HMAC is injective, so distinct counts don't change. Webmail membership is checked on cleartext. |
| `content.sent.first_link_host` | `etld1` | Cleartext if allowlisted, else HMAC | Text scorers see a token for unknown hosts (§12 Q14) |
| `resource.*.address_domain` | `etld1` (the default; revision 3's `none` exception is removed because no feature needs the full host) | Cleartext if allowlisted, else HMAC | No feature reads it |
| `content.sent.recipient_hash` and every `links` value (built-in and declared) | Re-HMAC. Each link kind's `join_domain` is its kind name. | — | Equality is preserved, so no value changes |
| `links.asn` | **Exempt** from the hash format check and from re-HMAC. It's coarse routing metadata in the clear (main §4.2), and its grammar tightens to `^(AS)?[0-9]{1,10}$`. | Cleartext | None |

**Subject ids**
- `account` ids stay as the tenant chose them, but they are now leak-scanned. An email, card or
  phone shape is rejected with `bad_subject`.
- Every other kind, and every `also` id, is stored as `hs<keyid>:` + HMAC(lp(kind) ‖ lp(id)).
- Lookups apply the same derivation.

**Ingest rules, in order**
1. **Input leak scan.** Every key and every **string** value is checked for:
   - email addresses;
   - Luhn-valid runs of 13–19 digits, with separators;
   - IPv4 and IPv6 literals;
   - phone shapes: a leading `+` followed by 8–15 digits, or a digit run broken by space, `-`, `.`
     or parentheses into the grouped national formats of 10 or more digits.

   A bare digit run with no `+` and no grouping is **not** a phone shape, so a 10-digit ASN
   passes. Declared `number` values are JSON numbers, not strings, and are exempt, as §5.6 says
   for `number`. Undeclared numbers are dropped before they're stored.

   The scan runs after NFKC and after folding every Unicode `Nd` digit to ASCII. A hit in a `text`
   field is masked. A hit in a `hash` field is exempt, because the value is replaced. Anywhere else,
   the event is rejected with `redaction_failed`.
2. **Declared fields** are handled by kind (table above).
3. **Undeclared fields** are dropped, numbers included.
   - The stored field names must match `^[a-z0-9_]{1,64}$`, at most 32 per event.
   - Names that fail the grammar or the leak scan are dropped and counted in
     `x_undeclared_dropped_n`.
   - An undeclared type keeps only `type`, `at` and `x_undeclared`.
4. **Egress scan.** The serialised row is scanned again before insert, with the same detectors,
   skipping the known `hk`, `hd` and `hs` prefixes and the mask markers. A hit rejects the event
   with `redaction_failed` and increments `abusekit_egress_scan_hits`; any hit means a redaction
   bug.
5. **Profile load** leak-scans every enum value, `in` list and set file. A hit rejects the profile
   with `vocab_invalid`.

**Keys: distinct per tenant by construction**

```go
type Keys interface {
    // Master returns the root secret for key id `id`, or the current one if id == "".
    Master(ctx context.Context, id string) (keyID string, secret []byte, err error)
    ReadIDs(ctx context.Context) ([]string, error) // current + previous during rotation
}

// Derive is the only way to obtain a tenant key:
//   k = HKDF-SHA256(secret, salt = "abusekit/v1", info = lp(tenant) ‖ lp(purpose))
func Derive(master []byte, tenant string, purpose Purpose) []byte
```

- **Purposes:** `hash`, `domain` and `subject`.
- **Adapters:** file (P3b) and cloud secret manager (P3d).
- **Test:** for every pair of example tenants and every purpose, the derived keys differ, and the
  same input yields different stored values.

**Rotation (P3d) is dual-key**
- For `max_lookback` (at most 30 days), ingest writes both `<field>` (new key) and `<field>__prev`
  (old key).
- `links` gets one row per key id.
- Readers use `__prev` until `read_flip_at`.
- The key id is recorded in `vocab_version` (`"<tenant>@<n>/k<id>"`).

**Dev and staging re-HMAC migration (P3b).** Production has no data yet (A1).
- abusekit never saw the raw values, so re-HMAC means HMAC-ing the stored producer value. That is
  exactly what ingest does from P3b on, so migrated rows equal newly ingested ones.
- A Go migration job runs under the tenant key and can resume by `seq`. It:
  - rewrites `links.hash` and the hash, domain and subject values in `events.links` and
    `events.data`;
  - leaves `events.body_hash` untouched. From P3b on, `body_hash` is a SHA-256 over a
    **key-independent canonical form**: the redacted event *before* pseudonymisation (declared
    fields after masking and reduction, hash and domain values as the producer sent them). Neither
    rotation nor the migration can break duplicate/conflict detection. The digest covers the whole
    body, so it can't be used to test a single field;
  - sets `vocab_version`.

**Vocabulary history lives in the config tree.** Each tenant has an append-only
`tenants/<t>/history/NNNN.yaml` in the private config repo, and CI runs `abusekit config check`.
The check enforces three rules:
- the vocabulary can only widen;
- a feature's `(name, version)` is never redefined;
- `effective_at` is monotonic.

At runtime, `tenant_config_versions` is only a guard.

**Warm-up.** A newly introduced feature is cold from `effective_at` until `effective_at` plus its
horizon (§5.7). This covers:
- a new custom feature, or a new version of one;
- a feature over a newly declared field or type;
- a new counter spec that is still backfilling.

While any input is cold, an advise rule is stored as shadow, with `warming_until`.

### 5.7 Evaluation classes, pass order, budgets, flags and flooding

Revision 2's shared byte and step budget, and its weighted truncation feature, are replaced by the
following.

**`start`, defined.** The subject's anchor instant is the first of these that exists:
1. `subject.created.account_created_at`, when the producer supplied it;
2. the `at` of the **first accepted** `subject.created` event. Later `subject.created` events never
   move it;
3. `first_received_at`: the minimum **server-assigned** `received_at` over accepted events.

The chosen value is then clamped: `start = min(chosen, first_received_at)`. A producer-supplied
anchor can therefore never place `start` after the moment abusekit first saw the subject.

**"First accepted"** means the event with the smallest `(received_at, producer, id)`. `received_at`
is assigned by the server once, at acceptance, and never changes. Event-time orderings (`at`-based
facts such as `first_success_at`, and every DSL "first") keep `(at, producer, id)`.

The third fallback replaces revision 3's `LEAST(at)`. With `LEAST(at)`, a flood dated in the past
could move `start` earlier by up to the skew allowance, or without bound under backfill scope. In
replay, `received_at = at`, so fixtures without `subject.created` keep their legacy `start`. P4a
lists and justifies any fixture whose `start` changes because events precede its
`subject.created`.

**(a) Onboarding facts are maintained at ingest.** `subject_facts(tenant, kind, subject)` is
updated in the same transaction as the event insert. **The fact row is a deterministic function of
the accepted event set, with each event's `received_at` held fixed.** Given the same events with
the same `received_at` values, any processing order or interleaving produces the same row. The
permutation test in §5.7 (a2) checks this. The update rules:

| Fact | Update |
| --- | --- |
| `first_seen_at` (legacy, informational), `first_received_at` | `LEAST(existing, new)` over `at` and over server `received_at`, respectively |
| `account_created_at`, `first_subject_created_at` | Taken from the first accepted `subject.created`: the smallest `(received_at, producer, id)`. A later-processed event with a smaller key replaces them, so the result doesn't depend on processing order. These feed `start` (above). |
| `first_success_at`, `first_success_key`, `first_success_funding` | On `payment.attempt{succeeded}`: replace when the event's `(at, producer, id)` is smaller |
| `payment_counts` | `{succeeded, declined, blocked}`: increment |
| `declines_before_first_success` | On a `declined` event with `at ≤ first_success_at` (or no success yet): increment. **Recount** whenever `first_success_at` changes, both from absent to `t` and from an earlier move. The recount is the Σ of the **hourly** decline counters for hours before `hour(t)`, plus one boundary-hour query over `[hour(t), t]`. The counters are a built-in `subject_counters` spec, **primary-subject-only** (see H-A below). The query uses a partial index on declined payment attempts: `(tenant, kind, subject, at) WHERE type = 'payment.attempt' AND data->>'outcome' = 'declined'`. Cost: one indexed SUM over at most 2,160 hourly rows (90-day retention), plus the declines inside a single hour. That bounds the part an attacker controls to one hour's events. |
| `first_paid_upgrade_at` | `LEAST` over `subscription.changed{status: active, amount_minor > 0}` |
| `subscription_change_count` | Increment |
| Declared `before_first` fact specs | The same pattern, generically. The email pack's `self_send_before_external` is one of these specs (from P-E1; before that, a transitional Go fact) |
| `name_brands`, `name_has_at`, `exempt_subject_brands` | Brand ids matched at ingest on `resource.*` names, with the integration-token gate applied |

- **Isolation.** Ingest transactions run at **READ COMMITTED**. On a serialization failure or a
  deadlock (`40001`, `40P01`), the transaction is retried up to 3 times with jittered backoff. If it
  still fails, the item is rejected as a whole-request `5xx`, which the producer's outbox retries.
  Idempotency on `(tenant, producer, id)` makes the retries safe.
- **Locking (a1).** Every ingest transaction runs in a fixed order:
  1. **Lock the facts row first.** `INSERT … ON CONFLICT (tenant, kind, subject) DO UPDATE SET
     seq = subject_facts.seq + 1 RETURNING *` takes the row lock (or `SELECT … FOR UPDATE` when the
     row already exists).
  2. Insert the event.
  3. Apply the updates, and recount when required.

  Counter-example to the unlocked version: transaction T1 moves `first_success_at` earlier and
  recounts, while T2 concurrently inserts a decline dated before the new `t`. Without the lock, T1's
  recount can't see T2's uncommitted decline, and T2 compares against the old `first_success_at`.
  Either way, one decline is lost or counted twice. With the lock, T2 blocks until T1 commits, then
  reads the new `t` and increments correctly. T2's event row can't commit before T2 holds the lock,
  so it's never counted twice.
- **Permutation test (a2).** Each fixture event's `received_at` is taken from the fixture, so it
  is fixed rather than wall-clock. For every fixture, and for 1,000 random permutations and
  concurrent interleavings of its events, the final fact row must be bit-identical. The runs use
  the Postgres adapter with parallel transactions, each event inserted with its assigned
  `received_at`.
- **No onboarding scan is ever byte-capped.** Scoring reads one facts row.
- **Frozen class N facts are invalidated and recomputed** whenever:
  - `start` changes; the row records the `anchored_start` its values used;
  - a backfill-scope event with `at` inside `[start, start + A)` is accepted, which sets
    `anchored_dirty`.
- **Which subjects get facts and counters.**
  - Every indexed subject (primary, `also`, `via_parent`) gets `subjects` row updates and
    `first_received_at`.
  - Onboarding facts (payment, subscription, brand, self-send) are updated **only for the primary
    subject**. They describe the acting account.
  - **Lifetime** counters are incremented for **every index row** whose `subject_kind` is in the
    counter spec's `subject_kinds`. That includes `via_parent` rows, so parent-level lifetime
    totals see child events, matching what class A queries see through `event_subjects`.
  - **H-A: recount-backing counters are primary-subject-only.** Two kinds of counter spec back a
    recount: the built-in hourly decline spec, and the counters behind every `before_first` fact
    spec. These are incremented **only** for the primary subject's index row. `also` and
    `via_parent` rows never touch them, because the facts they feed are primary-only too. So the
    incremental path and the recount path always count the same set of events.
  - **Test (`TestRecountParentChild`):** child `C` (an `api_key` whose parent is `P`) emits 5
    declines, then `P` gets its first success. Run once with and once without forcing a recount.
    `P.declines_before_first_success` must be identical both times (0: the declines are `C`'s, not
    `P`'s), and `C`'s own facts must be unaffected.
- **Custom `before_first` features** compile to a generic fact spec
  `{count: {type, where}, before: {type, where}}`, kept in the facts row with the same rules:
  lock-first, recount on `∞ → t` and on earlier moves, and primary-only hourly counters plus a
  boundary-hour query.
  The recount uses the partial expression index for the spec's `count` predicate.
- **Erasure and re-signup.**
  - A legal erasure (S3b) deletes the subject's facts row and counter rows, except that numeric
    facts and counters of `abusive`-labelled subjects are kept under main §4.4's 24-month basis.
  - A re-signup is a new subject id, with a new facts row and fresh counters. Churn evidence
    carries across only through retained link hashes (`core.linked_*`, `neighbours`), never through
    facts.
- **Counter expiry vs lifetime totals.** Counter day rows expire under the **same retention rule
  as the event rows they count** (main §4.11). "Lifetime" therefore always means "over retained
  events", which is exactly what the legacy scans computed.
  - A backfill computes from the events retained when it runs and records a `retained_from`
    watermark.
  - The counter spec is not warm until the backfill completes, and its lifetime values are defined
    over `[retained_from, now]`.
- A brand-list change bumps the facts spec version. A backfill job then recomputes from retained
  events, and the brand features stay cold until it finishes.

**The blocked-payment flood can't move onboarding.** Take 10,000 tiny `payment.attempt` events with
`outcome: blocked`.
- Each one only increments `payment_counts.blocked`, which no built-in feature reads.
- It leaves `first_success_*` unchanged, because that only moves on `succeeded`.
- It leaves `declines_before_first_success` unchanged, because that only moves on `declined`.
- It leaves `first_paid_upgrade_at` unchanged, because that only moves on `subscription.changed`.
- It leaves `start` unchanged.

So `core.upgrade_delay_min`, `core.upgraded`, `core.declines_before_first_success` and
`core.first_funding_prepaid` are bit-identical with and without the flood. Criterion 6 asserts
this. The same argument covers any flood of any type the update rules above don't name.

**(b) Lifetime totals come from counters.** Ingest increments
`subject_counters(tenant, kind, subject, spec_id, day, n, sum)`.
- Specs are compiled from the profile and pushed into the vocabulary, so ingest still consults only
  the vocabulary. They cover:
  - `resource.created`, all kinds;
  - `resource.created` with role `credential`;
  - `resource.created` + `activity` types (for `burst_ratio`);
  - declared lifetime `share` specs (the email pack's non-self recipients, all and webmail-only);
  - custom `lifetime` counts.
- A new spec backfills from retained events and stays cold until the backfill completes.
- Values are integers, so every sum is exact.
- No feature comes from a truncated scan of lifetime data, so **no feature can fall when history
  is bounded.** `TruncationDir`, its invariant and its packtest check are deleted.

**Evaluation classes and pass order** (per subject)

| Pass | Class | What runs | Budget |
| --- | --- | --- | --- |
| 1 | **F** | Read `subject_facts` and `subject_counters`. Evaluate neighbour evidence: `neighbours` exact by saturation (§5.5), and legacy `core.linked_*` with their frozen caps and flags. | O(1) rows, plus neighbour queries under their own examined-row budget. |
| 2 | **N** | Anchored `first:` features. While `now < start + A + 24h`, each runs its own dedicated anchored-range query over `[start, start + A)`. In P4a that query runs the legacy Go code over the anchored rows; from P4b it can use the class A engine. After that window, the value is **frozen** into `subject_facts.anchored`, stamped with `anchored_start`. It is recomputed when `start` changes or a backfill-scope event lands in the anchor (see (a)). | **Reserved.** Runs before A, R and G, and shares with nothing. Its row budget is 50,000; if hit, the feature is `partial` + `degraded`. |
| 3 | **A** | One exact aggregate query per feature: `count`, `sum`, `share`, `distinct`, `group_by`, `time_between`, `sequence`, the `cur` part of `relative_to_history`, and the class A parts of Go features. | **Per feature.** Returns O(1) or O(groups) rows. DB cost is O(rows in that feature's window), via the `(tenant, kind, subject, type, at)` index. |
| 4 | **R** | `relative_to_history` baselines first, then `peak`. Rows stream newest-first under each feature's own budget. | **Per feature.** Baselines: 50,000 rows by default. `peak`: LIMIT `N` from the saturation sizing below, capped by a 50,000-row budget. If that budget binds before `N`, the feature is `partial` + `degraded`. |
| 5 | **G** | Go-pack computation that isn't expressible as A or R. For e2a after §5.1 this is **empty**: every built-in feature is F, N, A or R. The class exists for future Go packs. | **Per feature.** Each G feature declares its own row budget in its `FeatureDef`. Hitting it sets the feature `partial` + `degraded`, because a Go feature's direction under truncation isn't proven. |
| 6 | **D** | `ratio` and the `__absent` indicators. | O(1) |

**Saturation sizing: why `peak` is exact under its limit, per transform.** Revision 3 sized the
limit in *post-transform* units, which is wrong. For example, `custom.declines_10m_peak`
(`log1p`, cap 7) needed `N = 7·144 = 1,008` rows under that sizing. Those rows could give a loaded
peak of 9, whose `ln 10 ≈ 2.3`, while the true value was `ln 1001 ≈ 6.9`. The limit must be sized
in **raw units**.

Let `x_sat` be the smallest raw peak at which the scored value reaches its maximum.

| Transform | `x_sat` |
| --- | --- |
| cap `C` only | `C` |
| `log1p: true`, cap `C` | `⌈e^C − 1⌉` |
| `log1p: {scale: s}` or `{anchored_at: n}`, cap `C` | `⌈e^(C/s) − 1⌉` |
| with `relative_to_history` (after its baseline `B` and age factor `d` are computed) | Scored value `T(min(P/max(B,1), rc)·d)`, where `T` is the transform above, so the maximum is `T(rc·d)`, reached when `P ≥ max(B,1)·min(rc, T⁻¹(C)/d)`. Hence `x_sat = ⌈max(B,1)·min(rc, T⁻¹(C)/d)⌉`. |

Every `T` is monotone non-decreasing, and so is `P ↦ min(P/B, rc)·d`. A raw peak at or above
`x_sat` therefore scores exactly the maximum, and below it the value is exact whenever the stream
is complete.

Stream matching rows newest-first with LIMIT `N = x_sat·⌈W/S⌉`:
- **"The limit binds"** is determined at run time, never from the static size of `N`: the query
  asks for `min(N, budget) + 1` rows, and it binds only if it returns more than `min(N, budget)`,
  meaning matching rows remained beyond the limit.
- Every loaded row carries at least one raw unit, because rows with addends ≤ 0 are excluded in
  the query. So if `N` binds (rather than the budget), the loaded rows carry at least `N` units.
- Split `W` into `⌈W/S⌉` slots of width `S`. By pigeonhole, some slot holds at least `x_sat`
  loaded units.
- The sub-window `(t − S, t]` ending at that slot's last loaded event covers the whole slot. So
  the **loaded** peak is at least `x_sat`, and the scored value equals the maximum, which is also
  the true value, because the true peak is at least the loaded peak.
- If the limit doesn't bind, the stream is complete and the value is exact.

For a history-relative `peak`, `B` is computed first (pass order). If `B` is partial, it is a
lower bound on the true baseline, so the `x_sat` sized from it is smaller than the true one:
- if the limit binds, `v1` saturates at `rc`, which is at least the true `v1`;
- if not, `P` is exact, and dividing by a smaller `B` only raises `v1`.

Either way the result is one-sided upward, flagged `partial`.

**Both verification counter-examples are exact under this sizing, because their streams run to
completion:**
- **`custom.declines_10m_peak`** (log1p, cap 7): `x_sat = ⌈e^7 − 1⌉ = 1,096`, so
  `N = 1,096 · 144 = 157,824`. The counter-example's stream is about 2,200 rows, which is below
  both `N` and the 50,000-row budget, so it completes. The peak is exact: `ln 1001 ≈ 6.9`.
- **`email.sends_10m_max`** (`B = 50`, `rc = 300`, no log1p, cap 300):
  `x_sat = 50·min(300, T⁻¹(C)/d) = 50·min(300, 300/d)`, which is 15,000 at `d = 1`, so
  `N = 2.16M`. The stream is 5,000 + 302·143 = 48,186 rows, again below `N` and the budget, so it
  completes. The value is exact: `v1 = 5,000/50 = 100`.

A budget can bind only when a real stream is longer than the budget at run time. Only then is the
feature `partial` + `degraded`.

**(c) Hitting a bound sets a flag; it's never a weight.**
- `Output.Partial` lists features whose own budget was hit. `core.history_truncated` no longer
  exists.
- Every partial source is classified by direction:

| Partial source | Direction | Flags |
| --- | --- | --- |
| `relative_to_history` baseline budget hit | Value can only rise | `partial` |
| History-relative `peak` whose limit binds after a partial baseline | Value can only rise | `partial` |
| In-memory `group_by` space-saving with `reduce: max` and positive sign | Value can only rise | `partial` |
| `peak` or anchored (class N) row budget hit before saturation | Value may undercount | `partial` + `degraded` |
| `neighbours` examined-row budget hit before `K` | Value may undercount | `partial` + `degraded` |
| Legacy `core.linked_*` fan-in cap hit | Value undercounts | `partial` + `degraded` |
| Space-saving with `count_gte`, or with a negative sign or weight | Value may undercount | `partial` + `degraded` |
| Class G budget hit | Direction unproven | `partial` + `degraded` |
| `ratio` with a partial `num` | Inherits `num`'s flags; with a negative ratio sign or weight, always `partial` + `degraded` | — |

  Revision 3 claimed that fan-in caps apply only to positively weighted features and therefore err
  upward. **That had the direction backwards:** an undercount lowers a positively weighted value.
  It is corrected above.
- **API.** Each signal gets `partial: ["custom.x", …]`, omitted when empty. `degraded` follows
  main §4.4 and is also set when any advise rule has a feature that may undercount. The subject
  gets `partial: true` when any advise rule's signal has partial features.
  - Callers can read a bare `partial` as "risk may be overstated, never understated".
  - `degraded` keeps its existing meaning: "don't trust a low score".
- **Uniform rules** record the flags like any rule. They are shadow-only, so the flags never
  affect a tier.

**(d) Budgets are per feature and per pack. Changing one feature never moves another.**
- A feature's value depends only on the facts, the counters, its own queries and its own budget.
- Anchored work is reserved and runs first; facts need no budget.
- No pass reads another feature's intermediate state. The one exception is class D, which reads its
  declared inputs' pre-transform values.
- **`TestFeatureIndependence`:** for every example profile and every feature `f`, delete, add or
  redefine every other custom feature `g`, one at a time, and assert that `f`'s bits are unchanged
  on every fixture. A Go-pack variant does the same by swapping pack versions.

**(e) The flood property: `risk(flooded, bounded evaluator) ≥ risk(flooded, unbounded reference)`.**

The *unbounded reference* is the naive evaluator from criterion 4: it scans every stored event, with
no limits and no facts. The generator injects floods of minimum-size events (the smallest valid
encoding of each type):

| Dimension | Values |
| --- | --- |
| Read types | Every type any feature reads, with field values that match and don't match its predicates |
| Unread types | Declared types no feature reads, and undeclared types |
| Onboarding types | `payment.attempt` with every outcome (including 10,000 tiny `blocked`), `subscription.changed`, duplicate `subject.created` |
| Placement | Entirely before, interleaved with, and after the real events. Timestamps land inside, at the edges of, and outside every feature window, including future-dated events within skew. |
| Volume | 1×, 10× and 100× each feature's saturation bound |

The property holds by construction, or the verdict is `degraded`:
- Classes F and A are exact, so the bounded value equals the reference. Class N is exact until
  its budget binds, which sets `degraded`.
- `peak` is exact by raw-unit saturation, or `degraded` when its budget binds first.
- `neighbours` is exact by saturation, or `degraded`.
- A partial `relative_to_history` value is never below the reference, and its weight is never
  negative.
- Space-saving errs upward only for `max` with a positive sign. Every other combination is
  `degraded`.
- `ratio` can't take a partial-capable `den`.
- Criterion 6b covers the anchor: `start` ignores `at` on everything except `subject.created`.

The test also checks it empirically for every built-in and DSL feature on every fixture.

**(f) Rollout.** e2a keeps today's unbounded `EventsForSubject` evaluation until all of P4a–P4c have
landed: facts, counters, classes A and R, flags, independence and the flood property. Only then is
bounded evaluation switched on for e2a (§9).

**Horizons feed the plan.** For each feature, the compiler derives the store range it queries:

| Feature | Store range |
| --- | --- |
| `window: W` | `(now − W, now]` |
| `first: A` | `[start, start + A)` |
| `time_between` | `(−∞, now]`, via indexed first/last-match queries |
| `sequence` | `a` rows from `(now − W − within, now]`; `b` rows from `(now − W, now]` |
| `relative_to_history` baseline | `(now − lookback, now − exclude_recent]` |
| `before_first` | Facts row (§5.7a), plus the hourly-counter sum and one boundary-hour query on recount |
| `lifetime` | `subject_counters` rows for the spec, minus an indexed `(now, +∞)` query for specs that exclude future-dated events |
| `neighbours` | `links` index `(tenant, kind, hash)` for each `via` key, with `where` applied, up to `K` distinct subjects, under the examined-row budget |

**Limits** (validated at load; also the fuzz oracle)

| Limit | Value |
| --- | --- |
| Custom features per tenant | 64 |
| Features per event type | 16 |
| Predicate depth / leaves | 2 / 8 |
| `in` values / set-file entries | 256 / 100,000 |
| Window / lookback | ≤ 30 d |
| `peak` window ÷ size | ≤ 1,440 |
| `group_by` `max_groups` (in-memory adapter) | ≤ 1,000 |
| Baseline row budget | 50,000 |
| `neighbours` features | 4 |
| `also` entries / declared link kinds | 3 / 8 |
| Declared `number` without `max` | rejected |

**Cost calibration.**
- The P4c benchmark measures cost end to end: store queries, decode, extraction and orchestration.
- It runs at the limits, with fixtures flooded to 100× saturation, and writes `cost_table.yaml`. CI
  fails if p99 exceeds 50 ms.
- Database work grows with flood volume through index range scans. §5.8's fair queue bounds its
  share of worker time.
- A subject whose last pass exceeded its budget is rescored at most every 10 minutes, with a
  metric. This changes latency, never values.

### 5.8 Profiles, rules, scheduling

**Profiles live in a private mount** (`--profiles`). This repo ships only:
- `examples/tenants/*`: the five fictional §7 profiles;
- `examples/tenants/reference/`: today's config, renamed. The golden replay runs it, and e2a's
  private profile starts as a copy of it.

**New validation codes.** These add to main §4.5's list. The whole profile is rejected, with every
error collected.

| Area | Codes |
| --- | --- |
| Packs | `pack_unknown`, `pack_requires` |
| Features | `feature_unknown`, `feature_not_enabled`, `feature_namespace`, `duplicate_feature`, `feature_version_reused` |
| DSL | `dsl_invalid` (with a JSON pointer) |
| Vocabulary | `vocab_invalid`, `vocab_incompatible`, `subject_kind_unknown` |
| Weights | `weights_unknown_feature`, `uniform_not_shadow`, `baseline_weight_negative` |

**Per tenant**
- Reload is atomic and isolated per tenant; `/healthz` reports `config_error{tenant}`.
- Rule sets are per tenant: `computeVerdict` and `currentRuleNames()` read the subject's tenant
  profile (tested in P2).
- The local scorer's version (`local@<sha256[:12]>`) is per tenant, in the verdict `model` field and
  in API signals.

**Stage gate (P1s).** `maxRiskByScorer` considers only advise-mode local rules. This changes
behaviour on `main`. e2a has no staged rules, so its golden doesn't change.

**Scheduling**
- **Fair queue.** Claims go round-robin across tenants, with weights (equal by default) and a
  per-tenant concurrency cap (4 by default). Main §4.8's priorities apply within a tenant.
- **Rescore-storm control:**
  - timer rescores come only from features that feed a non-shadow rule;
  - DSL features coalesce into buckets of `max(5 min, W/12)`, while built-ins keep 5 minutes;
  - each tenant has a timer budget, by default 20 × active subjects per hour. Once it runs out,
    timer rescores are deferred and counted;
  - event-driven scoring is never budgeted.

### 5.9 Weights, eval and bootstrap per pack

**Weights**
- Weights are per rule and keyed by namespaced name.
- The local scorer's math is unchanged; it sums in `FeatureDef.Order`.
- Starter weights ship at `config/packs/<pack>/starter.yaml`, with `sign:` on every weight.

**Uniform priors (shadow only)**
- Normalise each input: `x̂ᵢ = min(max(xᵢ, 0), Boundᵢ)/Boundᵢ`.
- Weight: `wᵢ = sᵢ·4/k`, where `sᵢ` is `PriorSign`, `prior_sign` or `absent_sign`.
- Bias: `bias = −2 + (4/k)·|{sᵢ = −1}|`.
- Risk therefore stays in `[0.12, 0.88]`.
- Using uniform priors in advise is rejected with `uniform_not_shadow`.

**Eval**
- `abusekit eval --profile <dir>` or `--pack <p>`.
- Floors gain a `profile:` field.
- The manifest gains `profile_sha`, `pack_versions` and `feature_key_space`.
- Replay uses the in-memory adapters for facts, counters and aggregates, which are
  conformance-tested against the Postgres adapters.

**`packtest`** runs for every pack in CI. It checks:
- signs;
- that zeroing a weight moves a fixture band or an isolated scenario;
- determinism under shuffled arrival;
- no leakage from the future;
- namespace and `Bound`;
- independence (§5.7 d);
- the flood property (§5.7 e).

**Held-out fixtures** live in `examples/tenants/<x>/fixtures/heldout/`.
- They're authored in a separate commit after the features are frozen, with non-overlapping seeds.
- CI runs criterion 2 on the held-out set only.
- A PR that touches both a scenario's features and its held-out fixtures fails CI.

**Bootstrap** for a new tenant:
1. Start with `core_starter`, `brand_starter` if relevant, and `custom_uniform`, all in shadow.
2. Collect labels.
3. Hand-tune the weights.
4. Pass the gate.
5. Promote.

### 5.10 Storage (expand-only)

**`events`**
- New columns `vocab_version` and `subject_kind`.
- New index on `(tenant, subject_kind, subject, type, at)`.
- Partial expression indexes per declared enum field that a feature reads, built with
  `CREATE INDEX CONCURRENTLY`.

**New tables**
- `event_subjects(tenant, subject_kind, subject, type, at, event_seq, via_parent)`: at most 8
  rows per event. Its index `(tenant, subject_kind, subject, type, at)` lets class A queries
  through `also` and `via_parent` rows run as index-bounded range scans before the join to
  `events` for pushed-down data predicates.
- `subject_facts`.
- `subject_counters(…, day, n, sum)`: expires with main §4.11's numeric retention.
- `tenant_config_versions`.

**Changed tables**
- `subjects` is keyed by `(tenant, kind, subject)`.
- `links` gains `key_id`.
- `corpus_examples` gains `feature_key_space`.
- `verdicts` gains `profile_sha`, `reason_version` and `partial`.

**S3b erasure must be vocabulary-aware.** It uses each row's `vocab_version` to know which fields
are raw text, skeletons or pseudonyms. It also clears `subject_facts` and `subject_counters`, apart
from the numeric retention for abusive subjects (main §4.4).

### 5.11 API surface summary

| Surface | Change | Compatibility |
| --- | --- | --- |
| `POST /v1/events` | Optional `subject_kind`, `also`, declared keys in the flat `links` map, and `x_` fields; pack-declared types such as `content.sent` (unchanged on the wire); declared types; the redaction in §5.6; account-subject leak scan (`bad_subject`) | The wire format is additive. Storage semantics change (pre-GA). |
| `GET /v1/subjects/{id}`, `evaluate` | `?kind=`; per-tenant `model`; namespaced reasons; `warming_until`; `partial` | Additive |
| Per-item codes | None new | Unchanged |
| `score --jsonl`, corpus loader | `feature_renamed`; `corpus-v2` | Breaks once, pre-GA (P1) |
| Config | Per-tenant profiles in a private mount | Breaks once, pre-GA |

**Rejected: declaring vocabularies at runtime over HTTP.** It would let a producer key widen its own
redaction boundary.

### 5.12 Alternatives considered

- **CEL.** It has no windowed aggregates, is a heavy dependency, costs per expression rather than
  per history, and gives poorer error messages. It may come back later as a `where` leaf (§12 Q9).
- **A custom expression language, a plugin ABI, or SQL features.** These bring a parser,
  unreviewable code, unbounded cost, or weak isolation.
- **Canonical-key bridge (revision 1).** Nothing stored needs it. Registry order gives bit-exactness
  without keeping two names alive.
- **Neutral `delivery.sent` (revision 1).** It is still email-shaped. Declared roles are neutral.
- **Hashing undeclared strings (revision 1).** A hash of unreviewed data is still personal data.
- **A shared byte and step budget with a weighted truncation feature (revision 2).** It had four
  problems:
  - onboarding facts could be displaced;
  - totals could fall under truncation;
  - one feature's change could move others;
  - truncation mixed a data-quality signal into risk.

  Facts, counters, exact aggregates and flags remove all four.
- **First-come `group_by` admission (revision 2).** Early decoys could take every slot. The review
  proposed space-saving; revision 3 goes further with exact aggregation, and keeps space-saving
  only for the in-memory adapter's memory bound.
- **Luhn on declared numbers (revision 2).** It rejected valid amounts. Declared numbers are
  reviewed and must carry `max` instead.

## 6. Edge cases and failure handling

- **Pack error (a bug).** Rules that read the pack are unscored with `feature_error` and marked
  `degraded`. Hitting a bound isn't an error; it sets `partial` instead.
- **Rejected profile.** The previous profile stays live. On a cold start the tenant's subjects are
  `unknown`, and `/healthz` is red.
- **Events that arrive before their declaration.** Their undeclared data is dropped. Counters and
  facts start from the declaration, and features stay cold until their horizon passes or a
  backfill finishes.
- **Undeclared kinds.**
  - An undeclared resource kind becomes `other`; `strict` rejects it instead.
  - An undeclared subject or `also` kind is rejected with `redaction_failed`.
  - An undeclared link kind is rejected with `bad_links`.
- **Absent fields.**
  - Predicates on absent fields are false.
  - `sum` uses `default`.
  - `share` and `ratio` use `if_empty`.
  - `time_between` and `sequence` use `if_absent` plus the `__absent` indicator.
  - A non-finite result is a pack error.
- **Out-of-order arrival.** Fact updates are monotone, with a recount triggered when the first
  success moves earlier. Counters are commutative.
- **Duplicates.** Facts and counters are updated only for a newly accepted event.
- **Future-dated events.** They're counted (legacy) or subtracted exactly (custom), and they
  schedule a rescore.
- **Key rotation.** Dual keys keep equality exact.
- **Parent and `also` fan-out.** At most 8 index rows and 8 dirty marks per event, coalesced by
  `dirty_seq`.
- **Hostile config or events.** There's no code or regex. Limits, per-feature budgets and exactness
  apply. Each partial source is either one-sided upward (`partial`) or flagged `degraded` (§5.7c).
- **Brand-list change.** The facts spec version is bumped and a backfill runs; the affected
  features stay cold until it finishes.

## 7. Genericity walk: five scenarios (fictional)

The products, ids and domains below are invented, and the timestamps are in 2031. They are
committed in P6b with dev and held-out fixtures. Throughout:
- durations use `log1p`;
- every `time_between` declares `if_absent` and `absent_sign`;
- every number declares `max`.

### 7a. File sharing: malware-distribution burst ("Driftbox")

```yaml
packs: [core@1, brand@1]
vocabulary:
  version: 1
  resource_kinds: {api_token: {role: credential}}
  types:
    share.link_created:
      role: activity
      fields:
        visibility:   {kind: enum, values: [public, org, private]}
        file_kind:    {kind: enum, values: [document, archive, executable, image, other]}
        folder_title: {kind: text, role: title, max_len: 120}
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
     window: 1h, transform: {log1p: true, cap: 9}}
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
                    until_now: false, if_absent: 1440},
     absent_sign: "-", transform: {log1p: true, cap: 7.3}, prior_sign: "-"}
rules:
  - {name: malware_burst, mode: shadow, scorer: local, weights: uniform,
     inputs: [custom.public_links_1h, custom.risky_link_share_24h, custom.distinct_downloader_nets_1h,
              custom.max_downloads_per_link_1h, custom.download_peak_vs_history,
              custom.signup_to_first_public_link_min, custom.signup_to_first_public_link_min__absent,
              brand.title_match],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

```json
{"id":"db-003","subject":"acct_example_db_1","type":"share.link_created","at":"2031-03-02T09:03:00Z","data":{"visibility":"public","file_kind":"archive","folder_title":"Invoice Center"}}
{"id":"db-005","subject":"acct_example_db_1","type":"share.downloaded","at":"2031-03-02T09:05:02Z","data":{"link_hash":"lk_4f1c9a0e7b2d11","downloader_ip24":"ip_9a1b2c3d4e5f66","downloader_is_owner":false}}
```

- **Declarative:** everything above.
- **Go-only:** file-content verdicts. The product emits these as `content.verdict`, and
  `core.verdict_max_24h` reads them.

### 7b. Marketplace: card testing ("Tallyport")

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
        amount_minor: {kind: number, min: 0, max: 100000000, integer: true}
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
  - {name: custom.small_charge_ratio_1h, version: 1, description: small ÷ all charges (pre-transform),
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
              custom.small_charge_ratio_1h, custom.declines_10m_peak, core.credential_velocity_1h],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
  - {name: tested_card, mode: shadow, scorer: local, weights: uniform, applies_to: [card],
     inputs: [custom.card_merchants_24h], labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

```json
{"id":"tp-101","subject":"acct_example_tp_7","also":[{"kind":"card","id":"card_example_c1"}],"type":"charge.attempted","at":"2031-06-10T02:14:01Z","data":{"outcome":"declined","decline_code":"incorrect_cvc","amount_minor":100,"card_hash":"cd_1a2b3c4d5e6f77"}}
```

- `card_example_c1` is stored as an `hs…` pseudonym.
- `x_primary_subject_hash` is derived at ingest for rows reached through `also`. It is the primary
  subject's pseudonym, under the join domain `subject:<kind>`.
- **Declarative:** everything above.
- **Go-only:** issuer/BIN intelligence and external network velocity. The product can emit these as
  `content.verdict` or enum fields.

### 7c. Chat or community: spam invites ("Hearthchat")

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
        link_host:    {kind: domain}              # etld1; cleartext only if allowlisted
    block.received:
      fields: {blocker_hash: {kind: hash, join_domain: member}}
features:
  - {name: custom.invites_10m_peak, version: 1, description: invites in busiest 10 min today,
     peak: {type: invite.sent, size: 10m}, window: 24h, transform: {log1p: true, cap: 7}}
  - {name: custom.external_invite_share_24h, version: 1, description: invites outside own communities,
     share: {type: invite.sent, match: {field: target_class, eq: other_community}}, window: 24h, transform: {cap: 1}}
  - {name: custom.invites_blocked_within_10m, version: 1, description: invitees who blocked within 10 min,
     sequence: {a: {type: invite.sent}, b: {type: block.received}, within: 10m, on: {a: invitee_hash, b: blocker_hash}},
     if_absent: 0, absent_sign: "-", window: 24h, transform: {log1p: true, cap: 6}}   # absent = no `a` events at all: benign
  - {name: custom.linked_invite_share_24h, version: 1, description: invites with links,
     share: {type: invite.sent, match: {field: link_host, exists: true}}, window: 24h, transform: {cap: 1}}
  - {name: custom.phone_siblings_7d, version: 1, description: accounts sharing a phone created this week,
     neighbours: {via: [phone_hash], where: {created_within: 7d}}, transform: {cap: 20}}
rules:
  - {name: invite_spam, mode: shadow, scorer: local, weights: uniform,
     inputs: [custom.invites_10m_peak, custom.external_invite_share_24h, custom.invites_blocked_within_10m,
              custom.linked_invite_share_24h, custom.phone_siblings_7d, brand.name_match,
              brand.title_match, core.linked_deleted_n],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

- **Declarative:** everything above.
- **Go-only:** message-text classification, which the product emits as `content.verdict`.

### 7d. Developer API: credential stuffing through customer API keys ("Keyforge")

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
     if_absent: 0, absent_sign: "-", window: 24h, transform: {log1p: true, cap: 6}}   # absent = no `a` events at all: benign
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
              custom.hosting_share_1h, custom.max_attempts_per_ip_1h, custom.attempts_vs_history],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

```json
{"id":"kf-9001","subject":"key_example_k3","subject_kind":"api_key","type":"auth.attempted","at":"2031-09-04T11:00:01Z","data":{"outcome":"failed","login_hash":"lg_8c7b6a5f4e3d21","client_ip24":"ip_1f2e3d4c5b6a77","client_asn":"hosting"}}
```

- The account-level `failures_1h` sees events from the account's keys through `via_parent` rows.
- **Declarative:** everything above.
- **Go-only:** breach-corpus checks, and ASN reputation beyond the enum.

### 7e. AI inference: free-tier farming ("Lumenloop")

```yaml
packs: [core@1]
vocabulary:
  version: 1
  link_kinds: {oauth_sub_hash: {evidence: true}}
  types:
    usage.recorded:
      role: activity
      fields:
        model_tier:  {kind: enum, values: [small, medium, large]}
        tokens:      {kind: number, min: 0, max: 2000000, integer: true}
        quota_state: {kind: enum, values: [ok, near_limit, exhausted]}
features:
  - {name: custom.signup_to_quota_exhausted_min, version: 1, description: minutes to exhaust free quota,
     time_between: {from: {type: subject.created}, to: {type: usage.recorded, where: {field: quota_state, eq: exhausted}},
                    until_now: false, if_absent: 10080},
     absent_sign: "-", transform: {log1p: true, cap: 9.3}, prior_sign: "-"}
  - {name: custom.large_model_token_share_24h, version: 1, description: tokens on large models (both sides summed),
     share: {type: usage.recorded, match: {field: model_tier, eq: large}, sum: {field: tokens, default: 0, cap_each: 200000}},
     window: 24h, transform: {cap: 1}}
  - {name: custom.tokens_10m_peak, version: 1, description: tokens in busiest 10 min,
     peak: {type: usage.recorded, sum: {field: tokens, default: 0, cap_each: 200000}, size: 10m},
     window: 24h, transform: {log1p: true, cap: 15}}
  - {name: custom.minutes_since_last_use, version: 1, description: idle time after last use,
     time_between: {from: {type: usage.recorded, anchor: last}, to: {type: abusekit.never}, until_now: true, if_absent: 0},
     absent_sign: "+", transform: {log1p: true, cap: 9.3}}      # hash_quantum defaults to 60 (until_now)
  - {name: custom.oauth_siblings_deleted, version: 1, description: deleted accounts sharing the OAuth identity,
     neighbours: {via: [oauth_sub_hash], where: {deleted: permanent}}, transform: {cap: 20}}
  - {name: custom.oauth_siblings_new_7d, version: 1, description: accounts sharing identity created this week,
     neighbours: {via: [oauth_sub_hash], where: {created_within: 7d}}, transform: {cap: 20}}
rules:
  - {name: free_tier_farm, mode: shadow, scorer: local, weights: uniform,
     inputs: [custom.signup_to_quota_exhausted_min, custom.signup_to_quota_exhausted_min__absent,
              custom.large_model_token_share_24h, custom.tokens_10m_peak, custom.minutes_since_last_use,
              custom.oauth_siblings_deleted, custom.oauth_siblings_new_7d, core.linked_deleted_n],
     labels: [benign, abusive], benign_label: benign, threshold: 0.7}
```

- `abusekit.never` is a reserved type that never occurs. Combined with `anchor: last` and
  `until_now`, it measures time since the last use.
- Absence has its own indicator, so an account that never exhausts its quota isn't read as the most
  benign case.
- `neighbours` uses only the declared `oauth_sub_hash`, because `device_hash` already feeds
  `core.linked_deleted_n`.
- **Declarative:** everything above.
- **Go-only:** prompt similarity across accounts, and non-linear interactions under uniform priors
  (§12 Q15).

### 7f. What the walk shows

| Need | Primitive |
| --- | --- |
| Per-entity maxima and counts | `group_by` (exact) |
| Cause, then effect within a time bound | `sequence`, and `time_between` with `__absent` |
| Rates | `ratio` over pre-transform values |
| Cross-account farms | Declared link kinds and `neighbours` (as of now) |
| Non-account actors | Subject kinds, `also` and `parent` |

Still Go-only across all five scenarios:
- content understanding;
- external reputation;
- cross-subject text similarity;
- non-linear interactions under uniform priors.

The first two already have a channel: `content.verdict` and enum fields.

## 8. Migration plan for e2a

- **Emitter:** no change. S6 emits `content.sent`.
- **Profile:** a byte copy of `examples/tenants/reference/`.
  - Packs: `packs: [core@1, brand@1, email@1]`. `email@1` is the YAML reference pack from P-E1 on.
    Before P-E1, email features come from the transitional Go code in #7.
  - The legacy vocabulary made explicit: `key: credential` with #7's aliases, and `agent: other`.
  - Weights and the private brand list stay in the private mount.
- **P0:** `abusekit eval --golden` extends the existing replay.
  - It runs the current unbounded evaluator, ordered by `(at, producer, id)`.
  - It records bits for feature values, `NextRescoreAt`, per-rule hashes, risks, tiers, the score
    and `Version()`.
  - The output goes to `eval/golden/reference-flat.jsonl`.
  - **P0 portability clarification (2026-09-30):** existing weighted sums and
    Go's optional AMD64 FMA math path differ in their last bits. Keep exact
    references for ARM64, AMD64 with FMA (`reference-flat.jsonl`), and AMD64
    without FMA (`reference-flat-amd64-no-fma.jsonl`), with numeric-profile
    metadata selected independently of replay outputs. All 8,671 point identities,
    features, hashes, timers, tiers, and flags agree; only risks/scores differ.
    Preserve scorer behavior and exact comparison on every profile. See
    `eval/golden/README.md` for reproduction, build settings, and CI coverage.
- **P1:** values, risks and tiers stay bit-exact under the rename map. Hashes, cassette keys,
  fake-scorer outputs and SHAs are recorded as changed once. The output goes to
  `reference-ns.jsonl`, and every later slice must match it exactly.
- **Later slices, each checked against that golden:**
  - P3a: masking `subject_line` for card, IP and phone.
  - P3b: re-HMAC and domain HMAC. Both are injective, and webmail domains stay in cleartext.
  - P4a: facts and counters replace scans.
  - P4c: bounded evaluation. The fixtures sit far below every saturation limit.

  Any fixture that changes is listed and justified in its slice.
- **Bounded evaluation reaches e2a only after P4a–P4c** (§5.7 f).
- **Email becomes configuration (P-N0, P-E1, P-E2).**
  - **P-N0**, the neutral cleanup (M3), comes first.
  - **P-E1** loads the embedded YAML email pack for e2a *alongside* the Go email features, under
    the shadow namespace **`emailshadow.*`**. `email_yaml` would break the §5.3 grammar (M8).
    - The namespace is bound at load time (`packs: [{pack: email@1.0.0#sha256:…, as:
      emailshadow}]`). It is never written into `pack.yaml`, so **the validated artifact is the
      pack SHA**, byte-identical from P-E1 to ship.
    - P-E1 emits a production metric, `abusekit_pack_shadow_mismatch_total{tenant, feature,
      reason}`, comparing each shadow value with its Go twin at every scoring round (M6).
  - **The P-E2 gate** (M6):
    - The golden replay is bit-identical for values, risks, tiers, input hashes and
      `NextRescoreAt`.
    - **And** the production mismatch metric has been zero for **N consecutive days** (Q31;
      proposed N = 7), except for the documented production-only divergences below, which the
      metric labels separately.
  - **P-E2 parity oracle** (M7): the **post-P4a** Go email code, meaning the Go features after
    they moved onto facts and counters, and not #7 as merged. #7's behaviour reaches the oracle
    only through the P0–P4a goldens, which already pinned it.
  - **Documented production-only divergences** (M6). The golden can't show these, so the shadow
    metric labels each one:
    1. **Budgets and `partial`.** The YAML path runs under the bounded evaluator's budgets
       (§5.7), and a real stream can bind where the Go path had no bound.
    2. **Counter expiry at the retention boundary.** Lifetime shares from counters expire by day
       bucket, while the legacy scan dropped rows individually. Within the boundary day the two
       can differ.
    3. **Set membership fixed at ingest.** `x_recipient_domain__in_webmail` is computed at ingest.
       A webmail-list change needs a backfill, whereas Go re-checked the list at score time.
    4. **IDNA normalisation** (above).
  - **P-E2 switch:** it happens once the gate passes, and only then are the Go email features
    deleted. e2a's scores are bit-identical throughout.
- **#7 merges first**, as the transitional Go implementation. It protects e2a before the
  migration, and it is the parity oracle for P-E2.
- **Rollback:** before S8, every slice can be reverted on its own. After S8, only the rename is
  irreversible without a rescore, which is why P1 comes first.

## 9. Slices

These come after #5 and #7 merge. **#7 merges first**, as the transitional Go implementation of the
email features, so e2a is protected before the migration. P1 lands before any S5 or S8 PR, enforced
as described in §5.2.

| # | Slice | Contents | Depends on | Done when |
| --- | --- | --- | --- | --- |
| P0 | Golden replay | `abusekit eval --golden`; `(at, producer, id)` order; `reference-flat.jsonl` | #5, #7 | Golden committed; flipping one weight's last bit fails it |
| P1 | One-time rename | Every §5.2 consumer and its test; `FeatureDef`; `core.Vector`; registry-order summation; fake-scorer re-baseline; `corpus-v2` + `feature_renamed` in `LoadSnapshotCorpus` and `score --jsonl`; reason v2; corpus key-space migration; cassette header; `TestNoProductionBeforeRename` (guards S5 and S8) | P0 | `reference-ns.jsonl` bit-exact for values, risks and tiers; grep test clean |
| P1s | Stage gate | `maxRiskByScorer` limited to advise-mode local rules | P1 | Stage tests pass; golden exact |
| P2 | Tenant profiles | Private-mount loader; reference profile; per-tenant reload, `/healthz`, rule sets and scorer version; fair queue and concurrency cap. All built-in features available to every tenant. | P1 | Golden exact; isolation and fairness tests |
| P3a | Vocabulary and scans | `internal/vocab`; kinds (numbers require `max`); roles; `x_` fields; declared-domain PSL, `etld1` and IP checks; card/IP/phone scans with digit folding; `subject_line` masking; egress scan; drop-undeclared with name grammar; account-subject leak scan; ASN grammar; profile-load scans | P2 | Criterion 5 (non-key parts); golden exact or deviations justified |
| P3b | Keys and re-HMAC | `internal/secret` (HKDF, file adapter); re-HMAC of hashes and links with `join_domain`; domain allowlist/HMAC (built-in and declared); pseudonymised non-account and `also` ids; `RedactionSchemaVersion` 3; dev/staging migration job; cross-tenant key test | P3a | Criterion 5 complete; golden exact; migration test |
| P3c | Config history | `history/`; `abusekit config check`; `tenant_config_versions` | P2 | History CI tests |
| P3d | Rotation and cloud keys | Dual-key write, read and flip; `__prev` fields; `key_id`; cloud secret-manager adapter | P3b | Equality exact across a simulated rotation; adapter contract test |
| P4a | Facts and counters | `start` precedence; `subject_facts` and `subject_counters`; the lock-first ingest transaction; READ COMMITTED with bounded retry; recount on `∞ → t` and on earlier moves (primary-only hourly decline counters plus a boundary-hour query on a partial index); `start` clamp and `(received_at, producer, id)` "first accepted"; onboarding, brand and self-send facts; generic `before_first` fact specs; webmail counters subtracting `(now, +∞)`; anchored freeze using its **own** anchored-range query (legacy Go code over `[start, start + A)`), with invalidation on `start` change or backfill; fact and counter subject assignment for `also`/`via_parent`; retention-aligned counter expiry; backfill with a `retained_from` watermark | P3a | Golden exact (or `start` deviations justified); blocked-payment and pre-dated flood tests (6b); the permutation and concurrent-interleaving test with fixture-assigned `received_at` (a2); the lost-decline race test; `TestRecountParentChild` |
| P4b | Aggregate engine (class A) | `internal/evalengine` (Postgres and in-memory); `count`, `sum`, `share`, `distinct`, `group_by`, `time_between` + `__absent`, `sequence`, `ratio`; pushdown and indexes; conformance | P4a | Reference equality on 10k histories; adapter equivalence |
| P4c | Class R, budgets, flags, flood | `peak` (with `sum`) under the raw-unit saturation limit per transform (§5.7); `neighbours` exact by saturation; the partial/degraded direction table; `ratio` partial propagation and `ratio_den_partial_capable`; `relative_to_history` with its baseline budget; `partial` in verdicts and the API; pass order; `TestFeatureIndependence`; flood generator and property; cost benchmark. **Then enable bounded evaluation for e2a.** | P4b | Criteria 4 and 6; golden exact under the bounded evaluator |
| P4d | Rescore control and warm-up | Proportional coalescing; timers only from non-shadow rules; per-tenant budget; warm-up | P4c, P3c | Storm and warm-up tests |
| P5 | Pack gating | Registry; the Go `core` and `brand` adapters and the `declared` (YAML) adapter; enablement; `brand.title_match` (needs the `title` role); brand lists moved to data; `packtest`; starter weights | P3a, P4c | Golden exact; `feature_not_enabled`; every pack passes `packtest` |
| P-N0 | Neutral cleanup (M3) | `reason.go` made generic; `pkg/abusekit` `Links` replaced in place by a map (a breaking change, noted in the release notes); `eval/neighbors.go` and `allLinkKinds` driven by declared evidence; data-driven `eval/gen`; brand word lists moved to data; config YAML moved to `examples/tenants/reference`, `packs/email` and `packs/brand` | P5 | Golden exact. The SDK's contract tests are updated to the map `Links`, and a wire test shows the new SDK emits bytes identical to the pre-change SDK for every legacy link kind. The changelog carries a breaking-change entry. |
| P-E1 | YAML email pack, in shadow | Embedded `packs/email/` plus the manifest. Primitives: baseline override (monotone ops, `sum`/`cap_each`), `distinct.on_missing`, the closed `compat` enum with conformance tests, lifetime `share`, `brand_match` with declarable exemptions and the per-role matcher variants, standalone `age_decay`, cross-field constraints, `rescore: legacy_v0`. The `subject_display_names` fact table; pack extensions of built-in types; the flat `links` map with its canonical serialisation; the `derived` column; `store: raw+skeleton`. The `content.sent` schema, `email_hash`, `email_domain_class` and `address_domain` move into the pack. Loaded as `emailshadow.*`, with the production mismatch metric. | P-N0, P4b, P4c | The pack loads from the embedded manifest. **`TestWireByteIdentity`**: stored `links` and `data` bytes, `body_hash` and error codes identical over the whole pre-P-E1 corpus. Golden exact (Go still drives scores). Every shadow feature bit-identical to its Go twin on the golden, including `NextRescoreAt` and input hashes. The H1 counter-example fixture passes. |
| P-E2 | Parity gate, switch, deletion | Preconditions:<br>• embedded packs;<br>• fail-closed ingest (`TestIngestFailsClosedOnConfigError`);<br>• the shadow-mismatch gate: N clean days, divergences labelled (M6).<br>Then:<br>• switch the binding `as: emailshadow` to `as: email`; the pack SHA is unchanged;<br>• delete the Go email features, `WebmailSet`, the `--webmail` flag, the `content.sent` matches, and the transitional built-in `content.sent` schema;<br>• turn on the neutrality CI and the runtime `Reads` filter.<br>**Ordered ops step:** the hosted profile pin (`email@1.0.0#sha256:…`) and the compose cleanup (the `--webmail` flag and its environment variable) ship **in the same release** as the deletion. | P-E1 and the N-day gate | Parity oracle is the post-P4a Go code (M7). Golden exact after deletion. `TestCoreIsDomainNeutral`, `TestNonEmailProfileEndToEnd`, `TestGoRegistryLint` and `Reads` enforcement green. |
| P5b | Core DSL parity | Every expressible *core* S2 feature re-expressed in the DSL (a conformance check; the core Go features stay, being neutral) | P4c | Bit-exact against the Go feature on every fixture |
| P6a | Link kinds, `neighbours`, subject kinds | declared link kinds in the flat `links` map; `neighbours` (as of now); dirty propagation for declared kinds; `subject_kind`, `also`, `parent`, `event_subjects` (≤ 8); `?kind=`; `applies_to`; `x_primary_subject_hash` | P3b, P4b, P5 | Contract tests; propagation and no-double-feed tests; golden exact |
| P6b | Scenarios, reference packs and bootstrap (packs embedded and SHA-listed in the manifest) | Five example profiles with held-out fixtures. **Non-email reference packs** `packs/card-testing/` (from 7b) and `packs/api-credential-abuse/` (from 7d), each in YAML with fixtures, floors and starter weights. Uniform priors; `--profile`; floors with `profile:` | P4d, P5, P6a, P-E2 | Criterion 2 on held-out fixtures; isolation check; both non-email reference packs pass `packtest` with no email pack loaded |
| P7 | e2a cutover | Private profile in the ops mount; hosted `config check` | P5, P4c, S8's mount | Golden exact against the private copy |

Two v0 slices interact with this plan:
- **S3b** must be vocabulary-aware. Its **done-when** includes a test that erasing a non-abusive
  subject deletes its `subject_facts` and `subject_counters` rows. A second test checks that an
  `abusive`-labelled subject keeps only numeric facts and counters under the 24-month basis. S3b
  is easiest after P4a.
- S6 is independent of all of the above.

## 10. Scalability and extensibility

- **Per subject:** facts and counters are O(1). Each feature runs one aggregate, costing O(window
  rows) in the database. Row features run under per-feature budgets. Plans are cached per
  `profile_sha`.
- **Floods:** a flood raises database scan cost, never values. The fair queue and the slow-subject
  rescore limit keep one subject from monopolising workers.
- **Tenants:** about 100 tenants share the fair queue. Metrics are labelled `{tenant, pack}`.
- **Storage:** one facts row per subject, one counter row per spec per active day, and at most 8
  index rows per event.
- **Extensibility:**
  - new Go packs, gated by `packtest`;
  - promoting a custom feature into a pack;
  - weight fitting on corpus v2;
  - a CEL `where` leaf.

## 11. Verification strategy

**Seams under test:**
- the profile loader;
- ingest (redaction, facts, counters);
- `feature.Extract`, with both aggregate-engine adapters;
- `abusekit eval --profile`.

**Checks:**
1. The golden replay: bit-exact in P1 (hashes change once), then exact in every later slice.
2. `packtest` for every pack.
3. DSL conformance: the naive reference against both adapters. Edge tables cover:
   - clipping;
   - `before_first`;
   - the `peak` saturation limit;
   - the space-saving fallback;
   - `__absent`;
   - counter subtraction of future events.
4. Ingest:
   - fact monotonicity under shuffled arrival;
   - the recount when the first success moves;
   - duplicates never touching facts;
   - the blocked-payment flood.
5. Redaction:
   - mask vs reject;
   - digit folding;
   - the egress scan;
   - undeclared-name handling;
   - re-HMAC equality and `join_domain` separation;
   - cross-tenant key inequality;
   - domain allowlist vs HMAC;
   - subject-id pseudonyms.
6. `TestFeatureIndependence` and the flood property.
7. The loader fuzzer (limits as oracle) and config-history CI.
8. Tenant isolation and scheduling.
9. HTTP contracts: `subject_kind`, `also`, `?kind=`, `partial`, `bad_subject`, `feature_renamed`.

**Likely regressions and what catches them**

| Regression | Caught by |
| --- | --- |
| Summation-order drift | The golden |
| A stage literal missed in the rename | The grep test and the stage test |
| Facts diverging from a scan | The P4a golden and the naive reference |
| Pushdown SQL diverging from the in-memory adapter | Adapter equivalence |
| PSL drift | A pinned snapshot and its test |

## 12. Open questions (owner decisions)

Where the re-review's (R3) answer differs from the earlier recommendation, both are shown.

1. **Rename vs bridge:** rename once in P1. The R3 review agrees and asks for a bit-exact golden.
   **Now:** registry-order summation makes it bit-exact, and the 1e-12 tolerance is deleted.
   Approve?
2. **Ordering:** P0 and P1 before any S5 or S8 PR, enforced in the plan and by a test. Approve?
3. **Undeclared data:** drop the values and keep only the names.
   - R3: also constrain the names.
   - **Now:** names must match `^[a-z0-9_]{1,64}$`, at most 32 per event.

   Approve?
4. **Re-HMAC of built-in fields:** `recipient_hash` and every link, with a dev/staging migration
   job. Approve?
5. **Legacy window quirks:** freeze them in `@1` and harmonise in `@2`. Unchanged. Confirm?
6. **Roles:** revision 5 had `credential`, `other`, `activity`, `title` and `self`. Revision 6 adds
   the field roles `display_name`, `destination` and `recipient` (§5.0). Confirm?
7. **Bounding:**
   - Revision 2: byte caps, a shared step budget and a weighted truncation feature.
   - R3: ingest facts, counters, per-feature budgets, and truncation as a flag.
   - **Now:** R3, plus exact per-feature aggregates.
   - **Revision 4:** `peak` is sized in raw units per transform, and `neighbours` is exact by
     saturation. Anything that can undercount (row, anchored, neighbour and G budgets;
     space-saving outside `max`+positive) sets `partial` + `degraded`.

   Confirm the 50,000-row budgets (baseline, `peak`, anchored, neighbour examined rows) and
   `max_groups` of 1,000?
8. **Bootstrap:** uniform priors, shadow-only, no fitting, held-out fixtures, and `__absent`
   indicators with `absent_sign`. Confirm?
9. **CEL:** later as a `where` leaf only, or a new design pass? Unchanged.
10. **Brand list:** can a tenant narrow it as well as extend it? Unchanged.
11. **Custom namespace:** `custom.*` per tenant, or `<tenant>.*`? Unchanged.
12. **Config history:** in the config tree, checked in CI; the database is only a guard. Approve?
13. **S6:** emits `content.sent`, unchanged on the wire. From P-E1, the email reference pack
    declares it, not the binary. Settled.
14. **Domains:**
    - Revision 2: public-suffix check, stored in cleartext everywhere.
    - R3: default to eTLD+1, keep cleartext only for allowlisted or popular domains, and HMAC the
      rest, including built-in `recipient_domain` and `first_link_host`.
    - **Now:** R3, **except** built-in `recipient_domain` uses `reduce: none` plus HMAC. Reducing
      it to eTLD+1 would merge distinct fixture domains and change
      `email.first_day_distinct_domains`, whereas HMAC is injective.

    Also: accept that text scorers see a token instead of an unknown `first_link_host`?
15. **Feature interactions:** should `ratio` gain a capped `product` form, or should that wait for
    fitted weights?
16. **Subject kinds:** `subject_kind` and `also` (at most 3), `via_parent` rows (at most 8 per
    event), and pseudonymised non-account ids. Approve?
17. **Stage gate:** only advise-mode local rules count, as its own slice (P1s). Approve?
18. **Profiles and keys:** a private mount, and a provider-agnostic `Keys` interface with HKDF per
    tenant and purpose. Approve?
19. **Rescore control:** proportional coalescing for DSL features only, timers only from non-shadow
    rules, and 20 × active subjects per hour. Confirm?
20. **`group_by` admission:**
    - Revision 2: first-come.
    - R3: space-saving.
    - **Now:** an exact `GROUP BY` in the aggregate engine, with space-saving only as the in-memory
      adapter's memory bound, flagged `partial`.

    Approve?
21. **Declared numbers:**
    - Revision 2: a Luhn check on integers.
    - R3: author-trusted, with `max` required and no Luhn.
    - **Now:** R3.

    Approve?
22. **`partial` flag:**
    - Revision 3: `partial` never set `degraded`.
    - Verification: that was wrong for undercounting sources.
    - **Now (revision 4):** a bare `partial` means the value can only have risen. Any source that
      may undercount also sets `degraded` (§5.7c table).

    Should callers still treat a bare `partial` like `degraded`?
23. **`start` precedence (revision 4, new):** `account_created_at`, then the first accepted
    `subject.created` `at`, then server `first_received_at`. This replaces `LEAST(at)`, so a
    pre-dated flood can't age an account. It may change `start` for fixtures whose events precede
    `subject.created`; P4a lists them. Approve?
24. **Key-independent `body_hash` (revision 4, new):** SHA-256 over the redacted body before
    pseudonymisation, so rotation and the re-HMAC migration never break duplicate/conflict
    detection. Approve?
25. **Dirty marks beyond the fan-in cap (revision 4, new):** the first 50 per key are marked in
    the ingest transaction, and the rest by a rate-budgeted background job. Confirm?
26. **Where reference packs live and how they are versioned.**
    - Revision 6 proposed `packs/<name>/` in the repo, loadable from the mount.
    - The review, adopted now: packs are **embedded in the binary** and addressed by content SHA
      through a release manifest.
    - A tenant may add packs only in **non-reserved** namespaces, and may **never shadow** a
      reference pack (`pack_shadowing`).
    - Semver applies as before, and the SHA goes into `profile_sha`.
27. **Are reference packs public?** Yes, as proposed, with starter weights and floors derived
    **only from synthetic fixtures**. Adopted.
28. **Pinning.**
    - Revision 6: `email@1` floating was allowed anywhere.
    - The review, adopted now: **advise-mode profiles must pin `name@x.y.z#sha256:<hex>`**.
      Floating (`name@1`, `name@1.4`) is allowed only for profiles with no advise rules (shadow)
      and in dev.
    - `config check` records the resolved SHA.
29. **Tenant-private YAML packs.** The review's answer, adopted: tenant packs get **no `compat`**
    and **no shadowing**, must follow the §5.3 grammar, and may extend built-in types **only with
    `x_`-prefixed names**.
30. **`compat` options.**
    - Revision 6: free-form options for reference packs.
    - The review, adopted now: a **closed engine enum**, each option with a conformance test, and
      a new option needs a design change.
    - v0: `window_end_closed`, `include_future`, **`before_first_include_future`** (new, H2) and
      `rescore_legacy_v0` (H5).
31. **The P-E2 clean-days gate (new).** How many consecutive days of zero unlabelled shadow
    mismatches in production before the Go email code is deleted? Proposed: 7.
32. **Go SDK links change. Decided (owner):** replace `pkg/abusekit` in place, with `Links` as a
    map. It's a breaking change on `main`, called out in the release notes and changelog. No
    parallel versions.
33. **`recipient_count` bounds (new, M2).** The email pack declares `recipient_count` with
    `min: 1, integer: true, max: 1000000`. The max must be at least e2a's largest per-message
    recipient count. Confirm the value against e2a's send limits before P-E1.

## 13. Changes from earlier revisions

### Revision 2 (after the adversarial review)

- Dropped the canonical-key bridge in favour of a one-time rename.
- Replaced the event-count bound and the wall-clock deadline.
- Added re-HMAC, dropping of undeclared data, public-suffix domain checks, card/IP/phone scans and
  skeleton-only custom text.
- Added `group_by`, `sequence`, `ratio`, `neighbours` and subject kinds, with a five-scenario walk.
- Dropped `delivery.sent`.
- Should-fixes:
  - warm-up;
  - dual keys;
  - rescore control;
  - a fair queue;
  - advise-only stage gates;
  - config history in the tree;
  - private profiles.
- Re-sliced the plan into P0–P7.

### Revision 3 (addendum, after the re-review of revision 2)

**B2: bounded evaluation.**
- (a) Onboarding values come from ingest-maintained `subject_facts`, with monotone updates and an
  indexed recount. No onboarding scan is byte-capped, and the blocked-payment flood provably moves
  nothing.
- (b) Lifetime totals come from `subject_counters`, so no feature can fall under truncation.
  `TruncationDir` is deleted.
- (c) Hitting a bound sets a `partial` flag on the signal and subject; it's never a weight.
  `core.history_truncated` and its invariant and `packtest` check are deleted.
- (d) Evaluation classes F, N, A, R, G and D run in a fixed pass order, with reserved anchored
  work and per-feature or per-pack budgets. `TestFeatureIndependence` guards this.
- (e) The flood property is restated against an unbounded reference, with a specified generator
  and a per-class exactness argument, including `peak` saturation.
- (f) Bounded evaluation reaches e2a only after P4a–P4c.

**B1: rename.**
- `feature_renamed` in `LoadSnapshotCorpus`, and `corpus-v2`.
- The fake scorer is re-baselined in P1.
- Registry-order summation makes the golden bit-exact, so the 1e-12 tolerance is gone.
- `Bound` is defined for velocity features and is never a cap.
- The 1,000 cap on totals is dropped.

**B3: redaction.**
- Declared numbers are author-trusted, with `max` required and no Luhn.
- Domains default to eTLD+1, stored in cleartext if allowlisted and HMACed otherwise, built-in
  fields included. The `recipient_domain` exception is explained in §12 Q14.
- Undeclared names have a grammar and a cap.
- Non-account and `also` ids are pseudonymised, and account ids are leak-scanned.
- Keys are HKDF-derived per tenant and purpose, with a cross-tenant test.
- An egress scan runs after digit folding.
- `subject_line` masking is clarified.
- ASN is exempt from hashing and gets a tighter grammar.
- A dev/staging migration job is added.
- Enum values and set files are scanned when a profile loads.

**B4: the DSL.**
- Horizons feed the store range.
- `__absent` indicators with `absent_sign`, and `log1p` durations.
- `ratio` uses pre-transform values.
- `group_by` is exact, with a space-saving fallback.
- `hash_quantum`, with a 60-minute default for `until_now`.
- `peak` accepts `sum`, and `share` sums both sides; both are stated explicitly.
- `neighbours` is evaluated as of now, matching `eval/neighbors.go`.
- Dirty-mark propagation covers declared kinds.
- Declared evidence no longer feeds `core.linked_*`.
- `event_subjects` is capped at 8 rows per event, with `via_parent`.

**Slices.**
- P1s is split out.
- P3 is split into P3a, P3b, P3c and P3d.
- P4 is split into P4a–P4d, and P5b is added.
- P5 now needs P3a and P4c, and P6a needs P3b.
- A test enforces that P1 lands first.

### Revision 4 (addendum, after verification of revision 3)

**Blocking fixes (P4a, P4c)**
1. **`peak` exactness.** The limit is now sized in **raw units** per transform (§5.7 saturation
   sizing):
   - `x_sat` is `C`, `⌈e^C − 1⌉` or `⌈e^(C/s) − 1⌉`.
   - With `relative_to_history`, `x_sat = ⌈max(B,1)·min(rc, T⁻¹(C)/d)⌉`, computed after the
     baseline.
   - `N = x_sat·⌈W/S⌉` rows. The pigeonhole argument is restated over loaded units.
   - If the row budget binds before `N`, the feature is `partial` + `degraded`.
   - Both verification counter-examples are worked through.
2. **`neighbours`.**
   - `where` is applied before any limit, and the query counts up to `K = ⌈T⁻¹(cap)⌉ + 1`, so the
     count is exact by saturation. An examined-row budget hit sets `degraded`.
   - Legacy `core.linked_*` caps now also set `partial` + `degraded`.
   - The backwards direction claim in §5.7c is corrected.
3. **`start` is defined** by precedence: `account_created_at`, then the first accepted
   `subject.created` by `received_at`, then server `first_received_at`.
   - Criterion 6b is restated.
   - Frozen class N facts carry `anchored_start` and are recomputed when `start` changes or a
     backfill-scope event lands in the anchor.
4. **Recount locking.**
   - The facts row lock is taken first (`INSERT … ON CONFLICT … RETURNING` / `FOR UPDATE`). The
     lost-decline race is given as the counter-example.
   - The recount costs O(days + boundary-day declines), using daily decline counters plus a
     partial index.
   - It fires on `∞ → t` and on earlier moves.
5. **Webmail counters** subtract future-dated events over `(now, +∞)`, matching legacy and
   covering backfill keys that are exempt from the skew check.
6. **`also`/`via_parent`.**
   - Onboarding facts are updated for the primary subject only.
   - Counters are updated for every index row whose kind is in the spec.
   - `event_subjects` gains `type` and `at`, with a matching index, so class A queries through
     parents are index-bounded.
7. **`ratio`.** Partial status propagates into class D. A partial-capable `den` is rejected at
   load (`ratio_den_partial_capable`).

**Text fixes**
- Space-saving direction: only `max` with a positive sign is upward. `count_gte` and negative signs
  are `degraded`.
- The facts row is restated as a deterministic function of the accepted event set, backed by a
  permutation and concurrent-interleaving test.
- The leak scan covers strings only, so declared numbers are exempt. Phone shapes need `+` or
  grouping, so 10-digit ASNs pass.
- `in_set`/`suffix_in_set` on domain fields are evaluated at ingest into a derived bool.
- Literals and set files on hash fields are HMACed at load, under both keys during rotation.
- `body_hash` is computed over a key-independent canonical form, so the migration no longer
  recomputes it.
- Class G budgets are per feature, with `partial` + `degraded` on a hit. G is empty for e2a.
- `distinct_recipients_1h` is consistently A (current) + R (baseline).
- §1 criterion 6 is restated relative to the unbounded reference, with the frozen dilution
  semantics (`burst_ratio`, webmail share) noted.
- Erasure and re-signup intent for facts and counters is specified. Counter expiry is aligned with
  event retention, and backfills carry a `retained_from` watermark.
- Load-plan rows are added for `before_first`, `lifetime` and `neighbours`, with a generic fact
  spec for custom `before_first`.
- `age_decay` features default to `hash_quantum` `0.01 × cap`.
- Dirty marks beyond the fan-in cap are handled by a rate-budgeted background job.
- The `address_domain` `none` exception is removed.
- The 7c/7d sequence `absent_sign` is `-`.

**Slices**
- P4a's anchored freeze uses its own anchored-range query, not the P4b engine.
- S3b's done-when includes the facts/counters erasure tests.
- `TestNoProductionBeforeRename` guards S8 as well as S5, by refusing production startup before
  the rename.

**Decisions**
- Q7 and Q22 are revised.
- Q23 (`start` precedence), Q24 (key-independent `body_hash`) and Q25 (dirty marks beyond the cap)
  are new.

### Revision 5 (addendum, after the final check of revision 4)

- **H-A: parent/child recount.**
  - The built-in decline counters, and every counter behind a `before_first` recount, are now
    primary-subject-only. `also` and `via_parent` rows never feed a recount.
  - `TestRecountParentChild` covers it: child `C` emits 5 declines, then parent `P` gets its first
    success, and `P`'s value is identical with and without a recount.
- **H-B: "first accepted" and determinism.**
  - "First accepted" means the smallest `(received_at, producer, id)`.
  - The determinism claim is now stated with each event's `received_at` held fixed.
  - The a2 permutation and concurrency tests take `received_at` from the fixture.
- **Fix 1: `peak` worked examples.**
  - Both counter-examples are exact, because their streams complete (about 2,200 and 48,186 rows).
  - "The limit binds" is decided at run time (more rows remained beyond the limit), never from the
    static size of `N`.
  - `x_sat` is corrected to `50·min(300, T⁻¹(C)/d)`.
- **Fix 4: ingest transactions and the decline recount.**
  - Ingest transactions run at READ COMMITTED, with up to 3 jittered retries on serialization
    failure or deadlock.
  - The boundary-day recount is replaced by hourly decline counters plus one boundary-hour query,
    so the cost an attacker controls is bounded to one hour's declines.
- **Fix 7: `ratio`.**
  - (a) Class N `first:` features join the `ratio_den_partial_capable` rejection list.
  - (b) A partial-capable `num` combined with a negative ratio sign or weight is flagged
    `partial` + `degraded`.
- **Recommended change adopted:** `start = min(chosen, first_received_at)`.

### Revision 6 (addendum: owner decision, a domain-neutral binary)

**Principle.** The compiled binary contains no domain-specific knowledge: no email and no e2a
concepts. §5.0 defines exactly what stays compiled:
- the engine;
- the built-in core vocabulary (subject lifecycle, payment, subscription, resource, verdict,
  label);
- the built-in neutral link kinds;
- the neutral Go packs `core` and `brand`.

**The email pack becomes configuration.**
- It becomes `packs/email/`: `pack.yaml` plus `webmail.txt`, starter weights, fixtures and
  floors. It is loaded by the same `declared` adapter as tenant `custom.*` features.
- The Go email features from #7 are transitional:
  - **P-E1** loads the YAML pack in shadow;
  - **P-E2** proves bit-for-bit parity on the golden replay, then deletes the Go email code in the
    same slice.
- New generic DSL primitives, so no Go special cases remain:
  - baseline override (own op, width and predicate);
  - `distinct.on_missing`;
  - versioned `compat` options (closed window end, include future-dated events);
  - lifetime `share`;
  - a `brand_match` op;
  - vocabulary cross-field constraints.

**Core vocabulary.**
- `content.sent` and its fields, the `email_hash` link kind, `email_domain_class` and
  `address_domain` all move into the email pack's declared vocabulary. The wire stays
  byte-compatible, via pack extensions of built-in types and a flat declared `links` map, which
  replaces `links.custom`.
- New field roles: `display_name`, `destination` and `recipient`.

**Core audit.**
- `core.burst_ratio` is generalised to `activity` types.
- `core.email_domain_class_disposable` moves to `email.domain_class_disposable`.
- The neighbour evidence set is declared rather than hard-coded.
- Self-send and webmail logic is now declared only.

**Brand pack.**
- The matcher, including its gates, stays Go. It is neutral string processing over `title` and
  `display_name` fields.
- The brand list, integration tokens and community phrases become data files.

**Neutrality CI.**
- `TestCoreIsDomainNeutral`: an AST denylist, with a justified allowlist.
- `TestNonEmailProfileEndToEnd`: the card-testing profile, with no email pack loaded.
- `TestGoRegistryLint`.

**Reference packs.** Besides `email`, P6b ships `card-testing` and `api-credential-abuse` in YAML.

**Slices.**
- #7 merges first, as the transitional implementation.
- P-E1 and P-E2 are added, and P5, P5b, P6a and P6b are revised.
- e2a's scores stay bit-identical throughout.

**Decisions.** Q6 and Q13 are revised. Q26–Q30 are new: where reference packs live and how they
are versioned, whether they are public, pack pinning, tenant-private packs, and `compat` options.

### Revision 7 (addendum, after the focused review of revision 6)

**High**
- **H1:** `brand_match` gains declarable exemption parameters (`exempt: {type, where,
  require_token, match_variant, live_unless}`).
  - The exemption is computed at score time from the `subject_display_names` fact table.
  - The engine pins the matcher variant per role: `title` has the community gate on and the
    integration gate off; `display_name` the reverse.
  - Matching runs after masking and truncation.
  - Standalone `age_decay = T(min(raw, T⁻¹(cap))·d)`.
  - The "Stripe API Key" / "Your Stripe invoice" counter-example gives `1·d` and is a committed
    parity fixture.
- **H2:** `self_send_before_external` uses `eq: false` for "external", so an absent field is not
  external. Adds `compat before_first_include_future`, also listed in Q30.
- **H3:** one explicit shared baseline, `B10`, for the four history-relative email features. The
  baseline override gains `sum`/`cap_each`, and baseline ops are restricted to monotone ones.
  This also covers L4.
- **H4:** reference packs are embedded and addressed by SHA. A tenant in `config_error` gets a
  retryable whole-request `503`, and nothing is dropped or rejected per item. Tested by
  `TestIngestFailsClosedOnConfigError`.
- **H5:** `hash_quantum: 0` on all nine features. Chosen: the pack-level `rescore: legacy_v0` mode,
  defined to equal Go's candidate set, so `NextRescoreAt` is asserted bit-exact.

**Medium**
- **M1:** the `links` serialisation is canonical: legacy order, then declared kinds sorted, and
  `{}` when empty. `email_hash` keeps `hex64`. An unknown key is still a whole-request 400.
  `TestWireByteIdentity` covers stored bytes, `body_hash` and error codes.
- **M2:** derived fields live in a separate `derived` column, outside `body_hash` and the 8 KiB
  limit. Adds `store: raw+skeleton`. `recipient_count` declares `min: 1, integer: true,
  max: 1000000` (Q33).
- **M3:** the audit of non-feature code is complete: `reason.go`, the SDK's `EmailHash`,
  `eval/neighbors.go`, `eval/gen`, #7's `--webmail` flag and env var (with an ops note),
  `WebmailSet`, the `content.sent` matches, `allLinkKinds`, `"agent"`, and the config YAML. The
  cleanup slice P-N0 is added.
- **M4:** the neutrality test now:
  - scans all Go, including under `packs/` and in `_test.go`;
  - splits identifiers on camelCase and snake_case, and lowercases them;
  - keys the allowlist by `(file, identifier)`, with an explicit `agent`/user-agent rule;
  - enforces `Reads` at runtime through a filtered event view;
  - allowlists only three leak-scanner identifiers in one file;
  - moves `emailMaskExemptKey` into the pack.
- **M5:** a pack is a reference pack only if its SHA is in the embedded manifest. Reserved names
  can't be shadowed. `compat` is a closed enum.
- **M6:** a production shadow-mismatch metric, an N-clean-days gate before P-E2, and documented
  production-only divergences (budgets and `partial`, counter expiry, ingest-time set membership,
  IDNA).
- **M7:** the P-E2 oracle is the post-P4a Go code.
- **M8:** the shadow namespace is `emailshadow`, bound at load, so the pack SHA is byte-identical
  from P-E1 to ship.

**Low**
- **L1:** the exact float expression for `age_decay`.
- **L2:** an unreachable cap for `first_day_distinct_domains`, `if_empty: 0` on
  `webmail_recipient_share`, and the IDNA note.
- **L3:** `display_name` is opt-in per resource kind.
- **L4:** covered by H3.

**Slices**
- P-N0 comes first.
- P-E1's done-when adds byte identity.
- P-E2 requires embedded packs, fail-closed ingest, the ordered ops pin and compose step in the
  same release, and the clean-days gate.

**Decisions**
- Q26–Q30 now carry the review's answers.
- New: Q31 (clean days), Q32 (SDK links change) and Q33 (`recipient_count` max).

### Revision 7a

- Owner decision on Q32: the Go SDK is replaced in place. `pkg/abusekit` `Links` becomes a map as a breaking change, noted in the release notes and changelog. There's no v2 module path and no retirement window.
