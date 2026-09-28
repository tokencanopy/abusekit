// Package eval implements abusekit's evaluation harness (design §4.10):
// a pure, importable Run over a Dataset/Rule/Scorer, the metrics the CI
// gate and `abusekit eval` both need, cassette record/replay so CI never
// calls a vendor, and the two corpus shapes design §4.6 describes.
//
// eval lives at the repo root (not under internal/) so it can be
// imported the same way design §4.10's "external frameworks" paragraph
// describes — a caller outside this module still reaches it through
// `abusekit score --jsonl`/`abusekit eval` (the CLI), but code inside
// this module (cmd/abusekit, tests, a future private-corpus runner living
// in this same checkout) can import it directly.
//
// eval never opens a network connection and never reads a product
// database: every input is a file (or stdin) already in hand. The one
// exception is `abusekit corpus export` (cmd/abusekit/corpus_cmd.go),
// which reads Postgres directly — that command lives in cmd/abusekit,
// not in this package, precisely so eval itself keeps its "no DB" grip.
package eval

// AbusekitVersion is recorded on every Manifest (design §4.10: "abusekit
// version and git sha"). There is no build-time version-injection
// convention yet in this repo (no ldflags, no VCS-stamped build info
// wired up) — this is a placeholder string for that eventual wiring, not
// a real release identifier yet. Manifest.GitSHA (see eval.go) is left
// empty for the same reason: nothing captures it today.
const AbusekitVersion = "v0-s4-dev"
