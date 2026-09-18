.DEFAULT_GOAL := check

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := -s -w -X github.com/gablooge/sluiceway/internal/appversion.Version=$(VERSION)

# sqlc runs from its pinned image, so nobody installs it and CI generates with the same version.
SQLC := docker run --rm -v "$(CURDIR)":/src -w /src sqlc/sqlc:1.31.1

.PHONY: check build test vet lint tidy sqlc sqlc-check

# What CI runs. An item in docs/backlog.md is not done until this is green.
check: tidy sqlc-check vet lint test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/sluiceway ./cmd/sluiceway

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run

# Fails if go.mod or go.sum would change, without changing them.
tidy:
	go mod tidy -diff

# Regenerate the typed queries after changing a migration or a queries.sql file.
sqlc:
	$(SQLC) generate

# Fails if the checked-in generated code is stale.
sqlc-check:
	$(SQLC) diff
