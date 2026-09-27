.PHONY: build test test-db lint gate fmt

# build compiles the abusekit binary.
build:
	go build ./...

# test runs every package's fast, DB-independent tests (go test's -short
# flag; internal/store's DB-backed tests skip themselves under it).
test:
	go test -short ./...

# test-db runs the full suite including internal/store's DB-backed tests.
# Point ABUSEKIT_TEST_DATABASE_URL at a scratch Postgres, or rely on the
# default postgres://e2a:e2a@localhost:5433/abusekit_test (the same local
# Postgres e2a's own testutil uses; the target database self-provisions
# if missing).
test-db:
	go test ./...

# lint checks formatting and runs `go vet`. No external lint tool is
# pulled in for S1, keeping the module's dependency footprint minimal per
# AGENTS.md; a stricter linter (golangci-lint or similar) can be added
# later without changing this target's contract.
lint:
	@fmtout="$$(gofmt -l .)"; \
	if [ -n "$$fmtout" ]; then \
		echo "gofmt needs to be run on:"; \
		echo "$$fmtout"; \
		exit 1; \
	fi
	go vet ./...

# fmt applies gofmt in place.
fmt:
	gofmt -w .

# gate is a placeholder until S4 (the eval harness and eval/floors.yaml
# land there). It exists now so CI and this Makefile already have a
# stable `make gate` target for S4 to fill in without a CI config change.
gate:
	@echo "make gate: placeholder — the eval harness and eval/floors.yaml land in S4 (see docs/plans/2026-09-27-v0-plan.md)"
