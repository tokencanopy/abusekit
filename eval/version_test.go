package eval

import "testing"

// TestGitSHA_PrefersBuildGitSHA is fix round T6's own acceptance test:
// gitSHA must prefer the -ldflags-stamped buildGitSHA over whatever
// runtime/debug.ReadBuildInfo reports, since the latter is wrong inside a
// nested git worktree (buildGitSHA's own doc comment) — this test only
// proves the precedence, not the ldflags wiring itself (that's the
// Makefile's `gate` target, exercised by hand: `make gate` then `git
// rev-parse HEAD` against a run.json's manifest.git_sha).
func TestGitSHA_PrefersBuildGitSHA(t *testing.T) {
	original := buildGitSHA
	t.Cleanup(func() { buildGitSHA = original })

	buildGitSHA = ""
	fromBuildInfo := gitSHA()

	buildGitSHA = "deadbeefcafe0000000000000000000000000000"
	if got := gitSHA(); got != buildGitSHA {
		t.Fatalf("gitSHA() = %q, want the stamped %q", got, buildGitSHA)
	}

	buildGitSHA = ""
	if got := gitSHA(); got != fromBuildInfo {
		t.Fatalf("gitSHA() with buildGitSHA cleared = %q, want it to fall back to the original build-info value %q", got, fromBuildInfo)
	}
}
