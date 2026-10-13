# Contributing to mcpcli

This repository is maintained with AI agents, and contributions are expected
to be produced by an agent following the workflow below. Humans review diffs;
agents do the typing. The binding rules live in `README.md` (user-facing
contract) and `CLAUDE.md` (architecture, conventions, testing) — this file
adds only the agent workflow and the skills to load. The upstream sources of
every skill listed here are in [SKILLS.md](SKILLS.md).

## Agent workflow

1. **Design first** for anything non-trivial: brainstorm with the maintainer
   and record the outcome as a spec under `docs/superpowers/specs/`
   (gitignored: specs and plans stay local).
2. **Plan**: write an implementation plan with TDD steps, exact file paths,
   signatures, and test names (`superpowers:writing-plans`).
3. **Execute task-by-task** (`superpowers:subagent-driven-development` or
   `superpowers:executing-plans`). Keep `WORKING_STATUS.json` (repo root,
   gitignored — never commit it) updated: `in_progress` + owner on start,
   `done`/`blocked` + notes on finish.
4. **Review with $ponytail** (`ponytail:ponytail-review`): on the plan before
   executing and on the diff before finishing. The diff's best outcome is
   getting shorter — cut hand-rolled code an existing package provides.
5. **Bump the version**: every substantive change bumps the **minor**
   version in `internal/version/version.go` (`v1.0.0` → `v1.1.0`). See
   [Versioning](#versioning).
6. **Verify before done** (`superpowers:verification-before-completion`):
   `go vet ./...` and `go test ./... -count=1` green, gofmt clean, output
   pristine.

## Versioning

The baseline version lives in `internal/version/version.go`
(`const Version`). `version.Current()` prefers the module stamp recorded by
the go tool — so a binary built from a release tag reports that tag — and
falls back to the constant, so local builds print the baseline instead of
`(devel)`.

- **Every substantive change bumps the minor version** of `Version`
  (`v1.0.0` → `v1.1.0`). Documentation-only fixes do not bump.
- Releases are cut by pushing an annotated tag `vX.Y.Z` matching the
  constant; the SLSA release workflow stamps the binary from it via build
  info.

## Skills to load (before touching the relevant area — not from memory)

**Process** (superpowers): `brainstorming`, `writing-plans`,
`subagent-driven-development`, `executing-plans`,
`verification-before-completion`, `requesting-code-review`,
`finishing-a-development-branch`. Review: `$ponytail`.

**Go conventions**: `golang-code-style`, `golang-naming`,
`golang-error-handling`, `golang-testing`, `golang-project-layout`,
`golang-context`, `golang-spf13-cobra`, `golang-lint`, `golang-modernize`,
`golang-stay-updated`. Load others (concurrency, security, …) when the
contribution actually touches them.

**Samber Go suite** — the preferred libraries when a contribution introduces
the matching need (none are current dependencies; adding one requires
maintainer approval first):
- `golang-samber-lo` — generic slice/map helpers
- `golang-samber-oops` — error wrapping with context
- `golang-samber-slog` — structured logging, if logging is ever added
- `golang-samber-do` — dependency injection, only if the project outgrows
  plain constructor wiring
