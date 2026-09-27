# abusekit

A small, model-agnostic abuse-scoring service and Go library. Products push typed events about a
subject (an account); abusekit keeps a risk score per subject with the signals behind it; products
read the score and decide what to do. It never enforces.

- Two integration calls: `POST /v1/events`, `GET /v1/subjects/{id}`.
- Rules are YAML; scorers are adapters (Jev, Laya, Gemini, ensembles); modes per rule (shadow, advise).
- Pure `score()` core, JSONL corpus, `abusekit eval` harness, CI gate on precision/recall/calibration.
- One binary (service) or importable package; Postgres or SQLite.

Design: `docs/design/2026-09-27-abusekit-design.md`. Status: design under review, no code yet.
