# eval/fixtures — synthetic replay fixtures

Every file here is entirely synthetic (public-repo data-boundary rule,
AGENTS.md): `.test`-suffixed domains, `acct_example_*` subjects, fictional
2031 timestamps, and no detail copied from the September 2026 incident this
design is modelled on beyond the general shape design §1 already describes
in public prose (fraud-declined attempts before a prepaid success, a churn
chain sharing a link key, a dormant-then-blast pattern). Plan amounts,
funding types, and brand names in every fixture are generic placeholders
chosen for test clarity, not a reconstruction of the real operator's
specifics.

## Link hashes

Every `email_hash`/`card_fingerprint_hash` value is `sha256(seed)` for a
short, descriptive, fixture-local seed string — never a real per-tenant
HMAC (design §4.2) and never derived from anything real:

```
$ printf '%s' 'churn-shared-email' | shasum -a 256
15cfb0373bfe14331f1bde1b55e8d46304f25c819608633b93dce835abe7603f
```

The seed for each hash actually in the fixtures:

| Fixture | Field | Seed |
| --- | --- | --- |
| `reference_operator.jsonl` | `email_hash` | `op-email` |
| `reference_operator.jsonl` | `card_fingerprint_hash` (declines) | `op-card-declined` |
| `reference_operator.jsonl` | `card_fingerprint_hash` (success) | `op-card-prepaid` |
| `benign_transactional.jsonl` | `email_hash` | `ben-email` |
| `benign_transactional.jsonl` | `card_fingerprint_hash` | `ben-card` |
| `burst.jsonl` | `email_hash` | `burst-signup-email` |
| `burst.jsonl` | `card_fingerprint_hash` (declines) | `burst-card-declined` |
| `burst.jsonl` | `card_fingerprint_hash` (success) | `burst-card-prepaid` |
| `benign_fast_onboarding.jsonl` | `email_hash` | `fast-onboarding-email` |
| `benign_integration_heavy.jsonl` | `email_hash` | `integration-heavy-email` |
| `dormant_then_blast.jsonl` | `email_hash` | `dormant-blast-email` |
| `churn.jsonl` | `email_hash` (all 12 subjects) | `churn-shared-email` |
| `churn.jsonl` | `card_fingerprint_hash` (all 12 subjects) | `churn-shared-card` |
| `fast.jsonl` | `email_hash` | `fast-signup-email` |
| `fast.jsonl` | `card_fingerprint_hash` (declines) | `fast-card-declined` |
| `fast.jsonl` | `card_fingerprint_hash` (success) | `fast-card-prepaid` |

Regenerate any of these with `printf '%s' '<seed>' \| shasum -a 256` rather
than editing the hex by hand — an earlier version of `burst.jsonl`,
`benign_fast_onboarding.jsonl`, `benign_integration_heavy.jsonl` and
`dormant_then_blast.jsonl` shipped with hand-typed hex patterns instead of
real `sha256(seed)` output (still validly formatted, 64 lower-case hex
characters, but not reproducible or documented); they were regenerated
against the seeds in the table above as part of this fixture-hygiene pass.

## Fixtures

- `reference_operator.jsonl` — a composite of the incident narrative's
  signals (fraud-declined attempts, a prepaid success, a quick upgrade, a
  resource burst, self-send rehearsal before an external blast). Kept from
  S2's original PR as the "everything at once" case.
- `burst.jsonl` — design §1 success criterion 2(a): reaches `high` from
  onboarding signals alone, at last-setup-event+15s, strictly before its
  first `content.sent`.
- `churn.jsonl` — design §1 success criterion 2(c): twelve subjects sharing
  a signup email hash and a card fingerprint, each created → paid → its
  first `resource.created` → a PERMANENT deletion; every subject from the
  third onward reaches `high` on its own first `resource.created`.
- `fast.jsonl` — design §1 success criterion 2(b) / plan.md's deferred-to-S3
  note: the same burst.jsonl shape (fraud-declined attempts, a prepaid
  success, a quick upgrade, a burst of agent/key creation, self-send
  rehearsal) compressed so its first EXTERNAL `content.sent` lands 30s
  after signup — "agents and first send inside one minute" — instead of an
  hour later. Replayed via the setup events only (everything before that
  first external send) against the synchronous `POST .../evaluate`
  endpoint (`internal/worker.TestEvaluateSubject_ReachesHighBeforeFirstSend`),
  not the worker's async Tick loop — this is what actually satisfies "fast"
  as distinct from `burst.jsonl`'s own async-within-15s criterion.
- `benign_transactional.jsonl`, `benign_fast_onboarding.jsonl`,
  `benign_integration_heavy.jsonl` — ordinary accounts (a slow real
  upgrade; a fast but unremarkable developer onboarding; an org wiring up
  several real SaaS integrations) that must never reach `high`.
- `dormant_then_blast.jsonl` — a week-old account with no upgrade that
  suddenly sends to 100 distinct external (non-webmail) domains within ten
  minutes; must reach `high`. S2b's own round 2 (R1) fix round stripped
  the brand-impersonating agent name and the resource-creation burst this
  fixture originally also carried, and raised its send volume from 20 to
  100 distinct recipients: the review required proof that volume/recipient
  signals ALONE — with no brand or resource evidence at all — still carry
  this shape to `high`, exercising `sends_10m_max`/`sends_1h`/
  `distinct_recipients_1h`'s history-relative `burstFactor` (see
  `internal/feature.burstFactor`/`priorTenMinutePeak`) directly: this
  subject has no prior sending history at all, so its burst reads at
  close to full strength.
- `benign_self_send_only.jsonl`, `benign_receipts_fanout.jsonl`,
  `benign_selfsend_brandname.jsonl` (round 2, R1) — three more accounts
  that must never reach `high`: a developer sending 8 test emails to their
  own inbox and never externally; a day-1 receipts account (1 agent)
  fanning out to 30 distinct real customer domains; an agent literally
  named "PayPal integration" that only ever self-sends (10 times). Each
  one previously scored `high` (0.94/0.96/0.99) on an UNCAPPED count
  feature alone (`self_send_before_external` or
  `first_day_distinct_domains`) — see `internal/feature`'s
  `selfSendBeforeExternalCap` (a hard cap) and
  `firstDayDistinctDomainsLogScale` (round 3, D2: a log1p(n) curve
  replacing R1's original hard cap, so volume above it is still
  meaningfully sensed instead of read identically to volume at the cap;
  `benign_receipts_fanout` now scores ~0.34, was ~0.15 under the hard
  cap — still comfortably medium/low either way).
- `benign_variant_a.jsonl` (round 2, R1) — a regression guard, not a new
  failure: 4 agents + 2 keys in 10 minutes, then 5 external emails to 5
  distinct domains within hour 1 (the re-review's own literal numbers,
  measured at 0.49/medium against the already-retuned round-1 weights).
  Committed so a FUTURE weight change (as opposed to R1's feature-level
  caps, which don't touch this fixture at all) can't silently push it into
  `high` without a test noticing.

- **S2b** adds four fixtures for the send-volume/webmail/recipient-hash/
  subject-brand-match feature family, each addressing a common
  bulk-phishing shape and each bounding at least one of the new weights
  (see `config/local_weights.yaml`'s own comments and the PR body's
  sensitivity-window table):
  - `webmail_blast.jsonl` — a brand-new account sending, within its first
    10 minutes, 100 webmail recipients' worth of mail on a single
    consumer webmail domain, with entirely neutral subject lines (no
    brand at all) — reaches at least `medium` on volume/webmail
    concentration alone.
  - `single_brand_blast_45m.jsonl` — a brand-new account, 240 webmail
    recipients over 45 minutes, every subject mentioning the identical
    fictional brand — reaches `high`.
  - `established_newsletter_burst.jsonl` — a 60-day-old paid newsletter
    with a real periodic sending history whose most recent send happens
    to burst 300 webmail recipients in 10 minutes — stays below `medium`
    (B1: volume alone must not flag an established sender, and the flag
    must decay once an account matures rather than persist as a lifetime
    fact — see round 2's R1 fixtures below for the history-relative
    mechanism that replaced the original hard age gate this fixture was
    first built against).
  - `day0_marketplace_seller.jsonl` — a brand-new account whose agent is
    named after a fictional shop brand, no integration token, sending to
    30 webmail buyers over its first hour — a plausible day-0 legitimate
    seller as much as a suspicious blast; also exercises S2 (the agent's
    own name already credits the brand, so the identical brand mentioned
    in every subject line must not ALSO count) — stays below `high`.

  These four use `eval/fixtures/test_brands.yaml`, an entirely fictional
  brand list (public-repo data-boundary rule, AGENTS.md: a replay fixture
  never references a real brand name), merged alongside the real,
  public `config/brands.yaml` via `feature.MergeBrandSets` wherever a
  test needs it (`internal/worker/mutation_test.go`'s `loadTestBrands`).

- **S2b's round 2 (R1)** replaced the send-volume features' original hard
  7-day calendar-age gate with a history-relative measure (`burstFactor`
  × `ageDecayFactor` — see `config/local_weights.yaml`'s own comments and
  `internal/feature.burstFactor`/`priorTenMinutePeak`/`ageDecayFactor`),
  proven evadable by simply waiting past it and blind to whether an
  "established" account had any real prior volume at all. Three more
  fixtures exercise the required outcomes directly:
  - `dormant_branded_burst_8d.jsonl` — an account dormant for 8 days (one
    day PAST the old gate) then bursting 100 branded webmail recipients
    within 10 minutes — reaches `high`: a calendar gate must not be
    evadable by waiting.
  - `paid_launch_5d.jsonl` — a 5-day-old paid SaaS account with a real
    (if modest) history of prior sends, pushing a neutral-subject launch
    announcement to 250 webmail recipients over 15 minutes — stays below
    `high` on the strength of that real prior history.
  - `webmail_spread_1h.jsonl` — 80 webmail recipients spread evenly
    across a full hour (8-minute intervals, deliberately NOT concentrated
    into any 10-minute window) — isolates `webmail_sends_1h`'s own
    contribution from `sends_10m_max`'s (which stays modest here), so the
    mutation sweep can prove `webmail_sends_1h` load-bearing on a REAL
    fixture rather than only a synthetic scenario (R6).

See `internal/worker/replay_test.go`, `replay_churn_test.go`,
`ablation_test.go` and `mutation_test.go` for what each fixture actually
asserts, and `config/local_weights.yaml`'s own comments for which fixture
bounded which weight.

- **S2b's round 2 (R3)** replaced the community-context gate's bare
  single-word list ("chat", "group", "fans", "club", "community",
  "meetup" individually — too broad, false-positiving on "<brand>: chat
  with support") with three whole PHRASES ("group meetup", "fan club",
  "community event"), and restricted it to subject-line matching only
  (never a resource/agent name, restoring name_brand_match for a name
  like "<brand> Support Chat"). `community_group_photo_walk.jsonl` is the
  required fixture: a day-0 community-group account posting an ordinary
  update with NO community phrase ("Fictabook photo walk this Saturday")
  to 80 webmail members in 10 minutes — see
  `internal/worker/replay_test.go`'s `TestReplay_CommunityGroupPhotoWalkKnownGap`
  for the documented trade-off this fixture landed on (it reaches `high`;
  the fixture is structurally close to indistinguishable, on this
  feature set alone, from a genuine brand-impersonation blast, and R1's
  blocker-level outcomes were kept intact rather than weakened to spare
  it).

- **S2b's round 2 (R5)** adds tier-envelope fixtures, each stated with
  margin >= 0.05 from the tier cut it lands on:
  - `single_brand_100_45m.jsonl` — a brand-new account, 100 webmail
    recipients over 45 minutes, one repeated fictional brand — `high`.
  - `brand_colon_country_variant_20m.jsonl` — a brand-new account, 60
    recipients on a country-variant consumer webmail domain
    (`hotmail.co.uk` — S8) over 20 minutes, subject line "Glowbank: your
    account was flagged" (the brand immediately followed by a colon —
    B3) — `high`.
  - `slow_sender_15_per_hour_6h.jsonl` — a documented KNOWN GAP, not a
    passing tier claim: the same total volume and brand mention as
    `single_brand_100_45m.jsonl`, paced at 15/hour over 6 hours instead
    of one burst, reaches only `medium` — see
    `internal/worker/replay_test.go`'s `TestReplay_SlowSenderKnownGap`
    and `docs/design`'s own §8 open-questions entry for why.
