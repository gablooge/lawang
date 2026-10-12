.DEFAULT_GOAL := check

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := -s -w -X github.com/gablooge/lawang/internal/appversion.Version=$(VERSION)

# sqlc runs from its pinned image, so nobody installs it and CI generates with the same version.
SQLC := docker run --rm -v "$(CURDIR)":/src -w /src sqlc/sqlc:1.31.1

.PHONY: check build test vet lint tidy sqlc sqlc-check tunnel acceptance-live

# What CI runs. An item in docs/backlog.md is not done until this is green.
#
# The live checks are deliberately NOT here and NOT in CI: they need real provider credentials,
# which CI does not have and must never have. See "Credentials for live verification" in CLAUDE.md,
# and the acceptance-live target below.
check: tidy sqlc-check vet lint test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/lawang ./cmd/lawang

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

# The public webhook tunnel, so a provider can reach this machine. It is started on demand and
# never as a service, and the token travels in the env file so that it is never on a command line.
# Cloudflare forwards ONLY ^/ingress/[a-z][a-z0-9_]{0,31}$$ to http://host.docker.internal:8080 and
# answers 404 itself for everything else; do not loosen that rule (CLAUDE.md).
#
# STOP THIS CONTAINER when the live check is done, and deregister every webhook that points at the
# hostname: one nobody deregistered goes on posting deliveries at a developer machine.
tunnel:
	docker run --rm --name lawang-tunnel \
	  --env-file ~/.config/lawang/cloudflare-tunnel.env \
	  cloudflare/cloudflared:2026.9.1 tunnel --no-autoupdate run

# The live half of the M1 acceptance suite: one real ClickUp delivery, signed by ClickUp, through
# the tunnel. It is the only thing that can show that Verify agrees with ClickUp, because a
# committed signature fixture is always one the tests signed themselves.
#
# It needs "make tunnel" running in another terminal, and three things in the environment:
#
#   LAWANG_LIVE_CLICKUP=1               the explicit opt-in
#   LAWANG_LIVE_CLICKUP_TEAM=...        the test workspace (team) id
#   LAWANG_LIVE_CLICKUP_TASK=...        a task in the designated test list
#
# Everything secret stays in LAWANG_CREDENTIALS_DIR (default ~/.config/lawang). A file or a
# variable that is absent is a skip that names it, never a default. One run creates one webhook and
# posts one comment, and removes both. See internal/acceptance/doc.go.
acceptance-live:
	go test -tags live -count=1 -v -timeout 20m -run 'TestLive' ./internal/acceptance/
