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
| `webmail_blast.jsonl` | `email_hash` | `webmail-blast-email` |
| `subject_lure_only.jsonl` | `email_hash` | `lure-only-email` |
| `benign_newsletter_webmail.jsonl` | `email_hash` | `newsletter-webmail-email` |
| `benign_newsletter_webmail.jsonl` | `card_fingerprint_hash` | `newsletter-card` |
| `benign_support_desk_webmail.jsonl` | `email_hash` | `support-desk-email` |
| `benign_shop_integration_subject.jsonl` | `email_hash` | `shop-integration-email` |

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
  suddenly creates ten resources (one brand-impersonating, "PayPal Account
  Alert" — not "...Bot": round 2's R6 made "bot" an integration-token that
  would otherwise suppress its own brand match) and sends to twenty
  distinct external domains within an hour; must reach `high`. The review
  that asked for this fixture described "300 external domains" — this
  uses 20, since no v0 feature (`first_day_distinct_domains` doesn't apply
  this many days after signup; `burst_ratio_24h_vs_lifetime` only cares
  that recent activity dominates lifetime activity, not the exact count)
  distinguishes 20 from 300 post-first-day domains.
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

- `webmail_blast.jsonl`, `subject_lure_only.jsonl` (**[S2b]**) — the two
  fixtures that must reach `high` on the [S2b] send-volume/webmail/
  subject-line-brand features alone, motivated by a real phishing
  campaign that scored low under the pre-[S2b] feature set: 1 agent + 1
  key, then 100 recipients on a single public webmail domain (gmail.com —
  a public provider name, not customer data) within ~8 minutes, brand
  words in the subject lines, no brand in the agent's own name
  (`webmail_blast.jsonl`); and neutral agent names, brand words in the
  subject line, 40 sends to a single NON-webmail domain over ~29 minutes,
  with no webmail signal at all (`subject_lure_only.jsonl`) — proving
  `subject_brand_match` plus modest volume is sufficient on its own,
  without any help from the webmail features.
- `benign_newsletter_webmail.jsonl`, `benign_support_desk_webmail.jsonl`,
  `benign_shop_integration_subject.jsonl` (**[S2b]**) — three more accounts
  that must never reach `high`, each isolating one [S2b] false-positive
  risk: a legitimate newsletter to webmail recipients (a 3-day-old account
  with a real credit-card payment, 200 sends over ~6 hours,
  `webmail_recipient_share=1.0`, no brand mention) proves webmail
  concentration alone isn't abuse; a support desk replying to webmail
  customers at a steady 20/hour on day 2 (past the first-day window)
  proves steady, modest-volume webmail traffic isn't abuse; a shop sending
  "Your Etsy order #N shipped" order confirmations through an agent
  literally named "Etsy Integration" (the existing integration-token gate
  already covers the agent-NAME side; `subject_brand_match=1` still fires
  on every subject line, since that gate is independent per field) proves
  a single repeated subject-line brand mention in an otherwise ordinary
  transactional pattern isn't abuse either.

See `internal/worker/replay_test.go`, `replay_churn_test.go`,
`ablation_test.go` and `mutation_test.go` for what each fixture actually
asserts, and `config/local_weights.yaml`'s own comments for which fixture
bounded which weight.
