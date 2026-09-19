.DEFAULT_GOAL := check

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := -s -w -X github.com/gablooge/sluiceway/internal/appversion.Version=$(VERSION)

.PHONY: check build test vet lint tidy

# What CI runs. An item in docs/backlog.md is not done until this is green.
check: tidy vet lint test

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
