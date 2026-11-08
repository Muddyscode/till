GO ?= go

.PHONY: test test-race vet build run lint

test:
	$(GO) test ./... -count=1 -timeout 180s

test-race:
	$(GO) test -race ./internal/ledger ./internal/httpserver ./tests/integration -count=1 -timeout 180s

vet:
	$(GO) vet ./...

build:
	$(GO) build -o bin/tilld ./cmd/tilld

run:
	$(GO) run ./cmd/tilld

