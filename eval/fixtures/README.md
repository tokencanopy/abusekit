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
- `benign_transactional.jsonl`, `benign_fast_onboarding.jsonl`,
  `benign_integration_heavy.jsonl` — ordinary accounts (a slow real
  upgrade; a fast but unremarkable developer onboarding; an org wiring up
  several real SaaS integrations) that must never reach `high`.
- `dormant_then_blast.jsonl` — a week-old account with no upgrade that
  suddenly creates ten resources (one brand-impersonating) and sends to
  twenty distinct external domains within an hour; must reach `high`. The
  review that asked for this fixture described "300 external domains" —
  this uses 20, since no v0 feature (`first_day_distinct_domains` doesn't
  apply this many days after signup; `burst_ratio_24h_vs_lifetime` only
  cares that recent activity dominates lifetime activity, not the exact
  count) distinguishes 20 from 300 post-first-day domains.

See `internal/worker/replay_test.go`, `replay_churn_test.go`,
`ablation_test.go` and `mutation_test.go` for what each fixture actually
asserts, and `config/local_weights.yaml`'s own comments for which fixture
bounded which weight.
