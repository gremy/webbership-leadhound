# Contributing to webbership-leadhound

This repository is public, and every push publishes at once.
Pushed history is never rewritten, so the rules below apply from the first commit, to every file, every commit message and every commit header.

## What never enters the repository

- Personal material of any kind: profiles, CVs, notes, applications, outreach, rates or anyone's job situation.
- Any real person: contacts, recruiters, or names that appear in collected pages.
- Secrets and infrastructure details: API keys, sending domains, server addresses, database dumps.
- Reasoning about the policies of any site we collect from, or about regulation.
  The code states what it does; the why behind those choices is out of scope here.
- Course or community material, links to it, or the names of its members.

The one sanctioned real name is the upstream licence attribution in `NOTICE`, which the upstream licence requires.

## Where private data lives

Private data stays outside the repository, and the binary reads it through `LEADHOUND_DATA_DIR`.
Never symlink into that directory, and never `go:embed` anything from outside the repository.
The repository ships a fictional demo in `examples/` instead.
Copy `.env.example` to `.env` for local settings; `.env` is ignored.

## Test fixtures

Fixtures are either synthetic or the output of `cmd/scrubfixture`, which replaces all text content and keeps the markup.
Raw collected pages never enter the repository; `testdata/raw/` is ignored for that reason.

Test data uses reserved names only: hosts and addresses under `.example` or `.invalid`, and IPv4 addresses from `192.0.2.0/24`.
A value that a check must block is built at runtime by string concatenation, never written whole in source.

## Hooks

Run `make hooks` once per clone.
It installs two hooks that run `cmd/privacycheck`:

- `pre-commit` checks the added lines and file names of the staged change;
- `commit-msg` checks the commit message.

The `pre-push` hook is left to the maintainer's own tooling and is never written or overwritten by this repository.

`privacycheck` reads a private denylist and a private allowlist.
Both files live outside the repository, with `allow.txt` in the same directory as the denylist.
The denylist path comes from `LEADHOUND_DENYLIST`, or else from the clone's own uncommitted git config (`git config leadhound.denylist <path>`), which every worktree of the clone shares.
A missing denylist fails closed.
Outside contributors, who do not have these files, set `LEADHOUND_DENYLIST=none` to run the generic checks only.

## Commits

Commit messages describe the code change only.
They never mention people, clients, money, or the author's circumstances.

Commits are authored with the project address in the private allowlist (or a `noreply` address).
That address appears only in commit headers and is never written into a file.

## Before any push

Run `make prepush`.
It checks every tracked file, every blob and message in the history and every author address, then runs a secret scan of the history.
A hit stops the push; fix it in a new local commit before pushing.

## Tests first

Write the failing test before the code that makes it pass.
`make check` runs formatting, vet, lint (when installed), the unit tests and the tracked-file privacy check.
`make test-integration` runs the database tests and fails, rather than skips, when the test database is missing.
