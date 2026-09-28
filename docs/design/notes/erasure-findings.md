# Findings: `GET /v1/subjects` (list) and `DELETE /v1/subjects/{subject}` (erasure)

Status: parked. This branch (`feat/s3b-list-erasure`) preserves the S3 PR's original
implementation of both endpoints (store `list.go`/`erasure.go`, the `internal/serve` handlers, the
`erase` key scope, migration `008_subjects_erased_at.sql`, the `pkg/abusekit` client's
`ListSubjects`/`Delete`, and their tests) exactly as it stood in PR #4 before the fix round split
them out. `docs/plans/2026-09-27-v0-plan.md` already lists both under "Out (v1)" — they were built
ahead of schedule in PR #4 because the task brief for that slice asked for them explicitly; the fix
round's two reviews found the erasure implementation specifically not safe to ship as-is, so it
comes back as its own slice (S3b) with a real design pass, not a revived copy-paste.

Two independent Opus reviews of PR #4 found the following, reproduced here (near-verbatim from the
consolidated fix list) so a future S3b design doesn't have to rediscover them from scratch:

## Erasure (`DELETE /v1/subjects/{subject}`)

1. **Tombstone wipes the wrong thing.** The tombstone path blanks `events.data` to `{}` for an
   abusive-labelled subject's own events. But `internal/feature.Extract` re-derives its entire
   feature vector from `events.data` on every scoring round — it does not read the numeric
   `corpus_examples.features` snapshot at all. Blanking `events.data` means the NEXT time the
   worker (or a synchronous evaluate) scores this subject, every feature it computes reads as zero:
   the subject silently rescoring to `low` and simultaneously stopping to count as evidence for its
   own same-tenant neighbours (`linked_deleted_n`/`linked_labelled_abusive_n` read the events table,
   not `corpus_examples`). A subject erasure was meant to destroy TEXT while keeping the numeric
   signal that justified the retention in the first place; as implemented it destroys the numeric
   signal too, just on a delay.
2. **No fence on `erased_at` anywhere it matters.** Ingest (`AppendEvents`), label writing
   (`PutLabel`), and the worker's own claim path (`ClaimDirtySubjects`) never check
   `subjects.erased_at`/`erasure_mode`. A new event for an erased (tombstoned) subject is accepted
   and stored in full — undoing the erasure's own effect on the very next write — and the worker
   will happily claim and rescore it.
3. **`labels.note`/`labels.evidence_ref` survive erasure untouched.** These can carry free-text
   evidentiary detail (an operator's note, an incident reference) and are not part of either the
   purge or the tombstone path's column list. A legal erasure request that leaves an operator's own
   free-text note behind is incomplete.
4. **The corpus row's subject-id hash is cosmetic.** `corpus_examples.subject` is replaced with
   `hashSubjectID(subject)` on the tombstone path, but the RAW subject id is still present, unerased,
   in `events.subject`, `links.subject`, `verdicts.subject`, `rule_state.subject`, `labels.subject`,
   and `subjects.subject` itself — every one of those rows is kept as-is. Hashing only the corpus
   row's own copy of the id does not pseudonymize the subject anywhere else it appears.
5. **The corpus hash is unkeyed and reversible.** `hashSubjectID` is `sha256("abusekit-erasure/" +
   subject)` with no secret key — for any candidate subject id an attacker (or a careless internal
   tool) can already guess or enumerate, this is a dictionary lookup, not a one-way pseudonymization.
6. **Idempotency is asymmetric and stateful in the wrong place.** A repeat tombstone call reports
   `already_erased:true` (the `subjects` row is kept, `erased_at` is checked). A repeat purge call
   404s (the row is gone) — which is fine on its own, but the ASYMMETRY means a caller can't write
   one idempotent retry policy for "erase this subject" without first knowing which path it will
   take, and there is no durable, query-able erasure LEDGER independent of whether the subject row
   itself still exists. A real design should record every erasure request (tenant, a keyed hash of
   the subject id, timestamp, mode) in its own table, independent of `subjects`, so an audit or a
   repeat-request check never depends on a row that a purge just deleted.

## List (`GET /v1/subjects`)

7. **Keyset order breaks the endpoint's own at-least-once paging guarantee.** The list is sorted
   `current_scored_at DESC, subject ASC` — newest-scored first. A subject that gets RE-scored (its
   `current_scored_at` advances) while a caller is mid-walk moves toward the FRONT of the sort order,
   which the caller has already passed — it can be skipped entirely rather than merely seen twice.
   Design's own "at-least-once paging" framing assumes forward progress never un-visits a row by
   having it move backward past the cursor; a DESC-on-a-mutating-column sort doesn't uphold that.
   Sorting ascending (oldest-scored-first, or by an append-only sequence) does: a row that changes
   during a walk can only move later in the remaining order, so it's seen again at worst, never
   skipped.

## What a real S3b design needs to settle before implementation

- A durable erasure ledger (its own table, keyed by `(tenant, keyed_hash(subject))`) as the source
  of truth for "has this subject been erased and how," independent of the `subjects` row's own
  lifecycle.
- A keyed (HMAC, server-secret) hash for both the ledger's key and any corpus pseudonymization —
  never a bare `sha256(subject)`.
- An explicit decision on WHICH tables get their raw subject id scrubbed on tombstone (all of them,
  per finding 4?) versus which are allowed to keep it under the same legitimate-interest basis that
  justifies retaining numeric features at all — this repo's current design doc (§4.4) only says
  "keeps numeric features and link hashes," which under-specifies `verdicts.subject`,
  `rule_state.subject`, `labels.subject` and `events.subject` themselves.
- A fence on `erased_at` in `AppendEvents`, `PutLabel`, and the worker's claim path, OR an explicit
  decision that a tombstoned subject is simply deleted from `subjects` entirely once erased (closer
  to purge, minus the corpus retention) so there is no "erased but still live" state for those paths
  to accidentally resurrect.
- A tombstone that removes/nulls whatever `internal/feature.Extract` actually reads (not just
  `events.data`) OR a documented acceptance that an erased-but-retained subject's score is allowed to
  decay to `low`/`unknown` going forward — and if the latter, an explicit decision on what that means
  for `linked_*` features on ITS neighbours (do they keep crediting a since-erased predecessor, or
  not?).
- List endpoint sort direction: ascending on an append-only ordering key (not `current_scored_at`
  DESC) so a mutating row can only be seen again, never skipped, during a paginated walk.

`docs/plans/2026-09-27-v0-plan.md` should gain an "S3b" row for this work once a design pass
addresses the above; migration number `008` is free again on `main` (this branch's own copy of
`008_subjects_erased_at.sql` is superseded, not renumbered, since a future S3b design may need a
different schema entirely).
