BINARY := leadhound
CMD    := ./cmd/leadhound

# Local Postgres (make db-local): the superuser is the macOS user over the local socket, and
# the local roles have no password. Override any of these on the command line.
LEADHOUND_MIGRATE_DATABASE_URL ?= postgres://leadhound_owner@localhost/leadhound_dev?sslmode=disable
TEST_DB_DSN                    ?= postgres://leadhound_app@localhost/leadhound_test?sslmode=disable
TEST_MIGRATE_DSN               ?= postgres://leadhound_owner@localhost/leadhound_test?sslmode=disable
TEST_SCALE_DSN                 ?= postgres://leadhound_app@localhost/leadhound_scale?sslmode=disable
TEST_SCALE_MIGRATE_DSN         ?= postgres://leadhound_owner@localhost/leadhound_scale?sslmode=disable

.PHONY: fmt fmt-check vet lint-if-present test test-integration test-scale check db-local migrate hooks privacy-tree prepush build-linux deploy deploy-data remote console-password console-local

# Server deploy (deploy/README.md). HOST is an ssh destination from your own ssh config; it
# never goes in this repository. ARCH is the server's CPU. LEADHOUND_PRIVATE_DIR holds your
# leadhound.yaml and ats-boards.yaml, outside this repository.
HOST                  ?=
ARCH                  ?= arm64
CONFIRM               ?=
ARGS                  ?=
LEADHOUND_PRIVATE_DIR ?= ../job-hunt/leadhound
SSH                   ?= ssh
SCP                   ?= scp
LINUX_BINARY          := bin/$(BINARY)-linux-$(ARCH)

# Refuse before anything runs, a dry run (make -n) included.
ifneq ($(filter deploy deploy-data remote,$(MAKECMDGOALS)),)
ifeq ($(strip $(HOST)),)
$(error HOST is required: make $(firstword $(filter deploy deploy-data remote,$(MAKECMDGOALS))) HOST=<ssh host> ...)
endif
endif
ifneq ($(filter deploy deploy-data,$(MAKECMDGOALS)),)
ifneq ($(CONFIRM),1)
$(error this changes the server: add CONFIRM=1 to run it)
endif
endif
# remote runs read-only commands freely; anything else writes or reaches the network
# (collect, canary, brief, score, migrate, recheck, probe, track <n>, rules add ...), so it
# needs CONFIRM=1 too. REMOTE_VERB is ARGS' first two words joined by _, e.g. track_list;
# REMOTE_VERB3 is the first three, for the leads commands whose third word is the verb.
empty               :=
space               := $(empty) $(empty)
REMOTE_READONLY     := track_list rules_list boards_list leads_suppress_list leads_suppress_check leads_batch_show
REMOTE_VERB         := $(subst $(space),_,$(strip $(wordlist 1,2,$(ARGS))))
REMOTE_VERB3        := $(subst $(space),_,$(strip $(wordlist 1,3,$(ARGS))))
ifneq ($(filter remote,$(MAKECMDGOALS)),)
ifeq ($(filter $(REMOTE_VERB) $(REMOTE_VERB3),$(REMOTE_READONLY)),)
ifneq ($(CONFIRM),1)
$(error remote ARGS='$(ARGS)' writes or reaches the network: add CONFIRM=1 to run it (free: track list, rules list, boards list, leads suppress list|check, leads batch show))
endif
endif
endif
# remote takes the lock the units take for the same work, without waiting: when a timer's
# run holds it, flock exits 75 and nothing runs (deploy/README.md).
REMOTE_LOCK :=
ifneq ($(filter collect_linkedin canary canary_% brief brief_% preview preview_%,$(REMOTE_VERB)),)
REMOTE_LOCK := /usr/bin/flock --nonblock --conflict-exit-code 75 /opt/leadhound/data/.linkedin.lock
endif
ifneq ($(filter score score_%,$(REMOTE_VERB)),)
REMOTE_LOCK := /usr/bin/flock --nonblock --conflict-exit-code 75 /opt/leadhound/data/.score.lock
endif
ifneq ($(filter leads_sites leads_contacts leads_agencies leads_transfer,$(REMOTE_VERB)),)
REMOTE_LOCK := /usr/bin/flock --nonblock --conflict-exit-code 75 /opt/leadhound/data/.leads-fetch.lock
endif
ifneq ($(filter $(ARCH),arm64 amd64),$(ARCH))
$(error ARCH must be arm64 or amd64, not $(ARCH))
endif

## fmt: format every Go file with gofmt -s
fmt:
	gofmt -s -w .

## fmt-check: fail if any Go file is not gofmt -s formatted
fmt-check:
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt needed (run make fmt):"; echo "$$out"; exit 1; fi

## vet: standard static checks
vet:
	go vet ./...

## lint-if-present: golangci-lint, integration- and scale-tagged files included, when installed; without it, say so and carry on
lint-if-present:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --build-tags integration,scale ./...; \
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

## test-scale: time and memory budgets on the production-sized leadhound_scale database (seeded once, then reused;
## -count=1 so every run measures the database, never a cached result)
test-scale:
	TEST_DB_REQUIRED=1 TEST_SCALE_DSN='$(TEST_SCALE_DSN)' TEST_SCALE_MIGRATE_DSN='$(TEST_SCALE_MIGRATE_DSN)' \
		go test -tags 'integration scale' -run 'Scale' -p 1 -count=1 -timeout 45m ./...

## check: everything a commit must pass
check: fmt-check vet lint-if-present test privacy-tree

## db-local: create the roles, their limits, and the dev, test and scale databases on local Postgres (idempotent)
db-local:
	psql -v ON_ERROR_STOP=1 -d postgres -f deploy/sql/roles.sql
	psql -v ON_ERROR_STOP=1 -d postgres -f deploy/sql/limits.sql
	@# Parallel test runs share the app role, so locally it has no connection cap; every
	@# other limit stays as on the server.
	psql -v ON_ERROR_STOP=1 -d postgres -c 'ALTER ROLE leadhound_app CONNECTION LIMIT -1'
	@for db in leadhound_dev leadhound_test leadhound_scale; do \
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

## console-password: ask for the console password twice (not shown) and write its hash to data/console-password.hash, mode 0600
console-password:
	@mkdir -p data && tmp=$$(mktemp) && \
	if go run $(CMD) console hash-password > "$$tmp"; then \
		install -m 0600 "$$tmp" data/console-password.hash && rm -f "$$tmp" && \
		echo "console-password: wrote data/console-password.hash"; \
	else rm -f "$$tmp"; exit 1; fi

## console-local: serve the console on loopback in dev mode, with .env's settings and the hash make console-password wrote
console-local:
	@test -f data/console-password.hash || { echo "console-local: run make console-password first"; exit 1; }
	@set -a; . ./.env; set +a; LEADHOUND_CONSOLE_PASSWORD_HASH=$$(cat data/console-password.hash) go run $(CMD) serve --dev

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

## build-linux: static Linux binary for the server, for ARCH (arm64 or amd64; default arm64)
build-linux:
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(LINUX_BINARY) $(CMD)

## deploy: make deploy HOST=<ssh host> [ARCH=arm64|amd64] CONFIRM=1: check, the scale test, build,
## copy the binary, backup script, role limits and units, migrate as the owner role, enable the timers
deploy: check test-scale build-linux
	@stage=$$($(SSH) $(HOST) mktemp -d) && \
	$(SCP) $(LINUX_BINARY) $(HOST):$$stage/leadhound && \
	$(SCP) deploy/install.sh deploy/leadhound-backup.sh deploy/sql/limits.sql deploy/leadhound-*.service deploy/leadhound-*.timer deploy/leadhound-*.path $(HOST):$$stage/ && \
	$(SSH) -t $(HOST) sudo sh $$stage/install.sh $$stage

## deploy-data: make deploy-data HOST=<ssh host> CONFIRM=1: check the profile and the leads config, then copy
## leadhound.yaml, ats-boards.yaml, leads.yaml, agencies.yaml and the leads-templates/ tree from
## LEADHOUND_PRIVATE_DIR to the server (nothing else); a template file no longer in the private tree is removed
deploy-data:
	@for f in leadhound.yaml ats-boards.yaml leads.yaml agencies.yaml leads-templates; do \
		test -e "$(LEADHOUND_PRIVATE_DIR)/$$f" || { echo "deploy-data: $$f not found in LEADHOUND_PRIVATE_DIR"; exit 1; }; \
	done
	LEADHOUND_DATA_DIR='$(LEADHOUND_PRIVATE_DIR)' go run $(CMD) score --check-config
	LEADHOUND_DATA_DIR='$(LEADHOUND_PRIVATE_DIR)' go run $(CMD) leads check-config
	@stage=$$($(SSH) $(HOST) mktemp -d) && \
	$(SCP) '$(LEADHOUND_PRIVATE_DIR)/leadhound.yaml' '$(LEADHOUND_PRIVATE_DIR)/ats-boards.yaml' \
		'$(LEADHOUND_PRIVATE_DIR)/leads.yaml' '$(LEADHOUND_PRIVATE_DIR)/agencies.yaml' $(HOST):$$stage/ && \
	$(SCP) -r '$(LEADHOUND_PRIVATE_DIR)/leads-templates' $(HOST):$$stage/ && \
	$(SSH) -t $(HOST) "sudo sh -c 'set -e; cd $$stage; \
		install -o root -g leadhound -m 0640 leadhound.yaml ats-boards.yaml leads.yaml agencies.yaml /opt/leadhound/data/; \
		find leads-templates -type d -exec install -d -o root -g leadhound -m 0750 /opt/leadhound/data/{} \\; ; \
		find leads-templates -type f -exec install -o root -g leadhound -m 0640 {} /opt/leadhound/data/{} \\; ; \
		cd /opt/leadhound/data; \
		find leads-templates -type f | while read -r f; do test -e $$stage/\$$f || rm -f \$$f; done; \
		rm -rf $$stage'"

## remote: make remote HOST=<ssh host> ARGS='track list' [CONFIRM=1]: run one leadhound
## command on the server as the leadhound user, with the units' environment. ARGS is split
## into words by your own shell, as on a command line; each word reaches leadhound whole,
## single-quoted for the server's shell. Anything but a read-only list needs CONFIRM=1.
remote:
	@$(if $(strip $(ARGS)),:,echo "remote: ARGS is required (for example ARGS='track list')"; exit 2)
	@set -f; set -- $(value ARGS); cmd=; \
	for a in "$$@"; do cmd="$$cmd '$$(printf '%s' "$$a" | sed "s/'/'\\\\''/g")'"; done; \
	$(SSH) -t $(HOST) "sudo systemd-run --quiet --wait --pipe --collect --uid=leadhound --gid=leadhound \
		-p EnvironmentFile=/etc/leadhound/env -p WorkingDirectory=/opt/leadhound \
		$(REMOTE_LOCK) /opt/leadhound/bin/leadhound$$cmd"
