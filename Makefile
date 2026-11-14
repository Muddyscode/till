GO ?= go

# The integration tests need a real Postgres. Set TILL_TEST_DATABASE_URL.
export TILL_REQUIRE_DB=1

.PHONY: test test-race vet lint build run

test:
	$(GO) test ./... -count=1 -timeout 900s

test-race:
	$(GO) test -race ./... -count=1 -timeout 900s

vet:
	$(GO) vet ./...

# Fails when any file needs gofmt.
lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

build:
	$(GO) build -o bin/tilld ./cmd/tilld

run:
	$(GO) run ./cmd/tilld
