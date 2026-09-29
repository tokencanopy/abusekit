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

import "runtime/debug"

// AbusekitVersion is recorded on every Manifest (design §4.10: "abusekit
// version and git sha"). There is no build-time version-injection
// convention yet in this repo (no ldflags) — this is a placeholder
// string for that eventual wiring, not a real release identifier yet.
const AbusekitVersion = "v0-s4-dev"

// SchemaVersion identifies run.json's own top-level shape (fix round S5)
// — independent of AbusekitVersion, which identifies the BINARY, not the
// wire format it emits. Bump it whenever a field on Manifest/Verdict/
// Metrics is renamed, removed, or retyped; an addition alone doesn't need
// a bump (this repo's own additive-change convention).
const SchemaVersion = "run.v1"

// buildGitSHA is empty by default; the Makefile's `gate` target sets it
// at link time via `-ldflags -X`. Fix round T6: `runtime/debug.
// ReadBuildInfo`'s vcs.revision reads the ROOT checkout's HEAD, not a
// nested `git worktree`'s own HEAD, when built from inside
// `.worktrees/<name>` (a Go toolchain limitation — the VCS stamping walks
// up to find a single repository root and stops there, never noticing
// the worktree's own separate HEAD) — every fix round in this PR was
// built from exactly such a worktree, so gitSHA() silently reported the
// wrong commit for every `make gate` run until this. -ldflags stamping
// bypasses the bug entirely: the SHA is baked in at build time from
// whatever `git rev-parse HEAD` the build's own working directory
// resolves, not re-derived from embedded VCS metadata at run time.
var buildGitSHA string

// gitSHA returns buildGitSHA when the binary was built with it stamped in
// (see buildGitSHA's own doc comment), else falls back to Go's own
// build-info VCS embedding (automatic since Go 1.18 when built from a git
// checkout via plain `go build`; see `go help buildvcs`) — correct for a
// normal, non-nested-worktree checkout, which is why the fallback is kept
// rather than replaced outright. Returns "" when neither is available (go
// run, a non-git checkout, -buildvcs=false, and no -ldflags stamping),
// same as AbusekitVersion's own placeholder status.
func gitSHA() string {
	if buildGitSHA != "" {
		return buildGitSHA
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
