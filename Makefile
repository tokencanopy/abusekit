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

# gate builds abusekit, runs `abusekit eval` against the committed
# synthetic corpus (eval/fixtures/synthetic/{events,labels}.jsonl) with
# the shipped local scorer, and fails (exit 1, readable diff on stderr)
# whenever a floor in eval/floors.yaml is violated — design §4.10 / plan
# S4's "make gate ... fails below eval/floors.yaml (floor = lower
# interval bound of the reference run)". Never touches Postgres, a
# vendor, or the network: the local scorer needs no cassette (S4).
#
# `go build ./...` (the `build` target above) deliberately writes no
# binary when it matches more than one package (Go's own default), so
# gate builds cmd/abusekit explicitly to a throwaway path instead of
# depending on `build`.
gate:
	go build -o .gate-abusekit ./cmd/abusekit
	@./.gate-abusekit eval \
		--dataset eval/fixtures/synthetic/events.jsonl \
		--labels eval/fixtures/synthetic/labels.jsonl \
		--rule new_account_velocity --scorer local --slice full \
		--floors eval/floors.yaml \
		--out .gate-run.json; \
	status=$$?; rm -f .gate-abusekit .gate-run.json; exit $$status
