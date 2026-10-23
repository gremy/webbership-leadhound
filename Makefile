BINARY := leadhound
CMD    := ./cmd/leadhound

# Local Postgres (make db-local): the superuser is the macOS user over the local socket, and
# the local roles have no password. Override any of these on the command line.
LEADHOUND_MIGRATE_DATABASE_URL ?= postgres://leadhound_owner@localhost/leadhound_dev?sslmode=disable
TEST_DB_DSN                    ?= postgres://leadhound_app@localhost/leadhound_test?sslmode=disable
TEST_MIGRATE_DSN               ?= postgres://leadhound_owner@localhost/leadhound_test?sslmode=disable

.PHONY: fmt fmt-check vet lint-if-present test test-integration check db-local migrate hooks privacy-tree prepush build-linux

## fmt: format every Go file with gofmt -s
fmt:
	gofmt -s -w .

## fmt-check: fail if any Go file is not gofmt -s formatted
fmt-check:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt needed (run make fmt):"; echo "$$out"; exit 1; fi

## vet: standard static checks
vet:
	go vet ./...

## lint-if-present: golangci-lint when installed; without it, say so and carry on
lint-if-present:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "lint: golangci-lint not installed (brew install golangci-lint); skipped"; \
	fi

## test: unit tests with the race detector
test:
	go test ./... -race

## test-integration: integration tests against the local test database; fails instead of skipping
test-integration:
	TEST_DB_REQUIRED=1 TEST_DB_DSN='$(TEST_DB_DSN)' TEST_MIGRATE_DSN='$(TEST_MIGRATE_DSN)' \
		go test -tags integration -p 1 ./... -race

## check: everything a commit must pass
check: fmt-check vet lint-if-present test privacy-tree

## db-local: create the roles and the dev and test databases on local Postgres (idempotent)
db-local:
	psql -v ON_ERROR_STOP=1 -d postgres -f deploy/sql/roles.sql
	@for db in leadhound_dev leadhound_test; do \
		if psql -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$$db'" | grep -q 1; then \
			echo "db-local: $$db exists"; \
		else \
			psql -v ON_ERROR_STOP=1 -d postgres -c "CREATE DATABASE $$db OWNER leadhound_owner" || exit 1; \
		fi; \
		psql -v ON_ERROR_STOP=1 -d $$db -f deploy/sql/grants.sql || exit 1; \
	done

## migrate: apply the migrations to the dev database as the owner role
migrate:
	LEADHOUND_MIGRATE_DATABASE_URL='$(LEADHOUND_MIGRATE_DATABASE_URL)' go run $(CMD) migrate

## hooks: install the pre-commit and commit-msg privacy checks (never touches pre-push)
hooks:
	@dir=$$(git rev-parse --git-path hooks) && mkdir -p "$$dir" && tmp=$$(mktemp) && \
	printf '%s\n' '#!/bin/sh' 'exec go run ./cmd/privacycheck staged' > "$$tmp" && \
	install -m 0755 "$$tmp" "$$dir/pre-commit" && \
	printf '%s\n' '#!/bin/sh' 'exec go run ./cmd/privacycheck msg "$$1"' > "$$tmp" && \
	install -m 0755 "$$tmp" "$$dir/commit-msg" && \
	rm -f "$$tmp" && echo "hooks: installed pre-commit and commit-msg in $$dir"

## privacy-tree: check every tracked file against the denylist and the generic patterns
privacy-tree:
	go run ./cmd/privacycheck tree

## prepush: the full gate before a push: tree, history and a gitleaks scan of the history
prepush: privacy-tree
	go run ./cmd/privacycheck history
	gitleaks git --no-banner .

## build-linux: static ARM64 Linux binary for the server
build-linux:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/$(BINARY)-linux-arm64 $(CMD)
