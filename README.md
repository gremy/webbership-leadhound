# webbership-leadhound

- **What**: a single Go binary, `leadhound`, that collects job postings from public sources, filters and scores them, and sends one daily brief by email.
  It runs as one-shot subcommands under systemd timers, with Postgres as its store; the application's database role cannot delete or drop.
- **How to run**: copy `.env.example` to `.env` and fill it in, then use the make targets.
  - `make check`: format check, vet, lint (when installed), unit tests and the tracked-file privacy check.
  - `make test-integration`: database tests against the local `leadhound_test`.
  - `make db-local`: create the roles and the local dev and test databases.
  - `make migrate`: apply migrations to the dev database as the owner role.
  - `make hooks`: install the pre-commit and commit-msg privacy hooks.
  - `make prepush`: the full gate before a push.
  - `make build-linux`: static ARM64 Linux binary.

  Subcommands (planned for slice 1): `collect`, `score`, `routes`, `brief`, `track`, `rules`, `canary`, `recheck`, `eval`, `boards` (`verify`, `add`), `probe company` and `migrate`, run as `go run ./cmd/leadhound <subcommand>`.
  Without `LEADHOUND_LIVE`, no command fetches from a job source.
- **Contributing**: see `CONTRIBUTING.md`; ported work is credited in `NOTICE`.
- **State**: see `STATE.md` (the head-pointer for where this left off).
