# Skills used in this repository

Upstream sources of the skills [CONTRIBUTING.md](CONTRIBUTING.md) tells AI
agents to load. Repo-wide agent instructions: [AGENTS.md](AGENTS.md).

## Process — [obra/superpowers](https://github.com/obra/superpowers)

Loaded as the `superpowers:` plugin; each skill lives under `skills/<name>/`:
`brainstorming`, `writing-plans`, `subagent-driven-development`,
`executing-plans`, `verification-before-completion`, `requesting-code-review`,
`finishing-a-development-branch`.

## Review — [DietrichGebert/ponytail](https://github.com/DietrichGebert/ponytail)

Loaded as the `ponytail:` plugin ($ponytail): `ponytail-review` — runs on the
plan before executing and on the diff before finishing; the diff's best
outcome is getting shorter.

## Go conventions — [samber/cc-skills-golang](https://github.com/samber/cc-skills-golang)

Installed as user-level skills (`~/.zcode/skills/golang-*/`, one directory
per skill in the repo): `golang-code-style`, `golang-naming`,
`golang-error-handling`, `golang-testing`, `golang-project-layout`,
`golang-context`, `golang-spf13-cobra`, `golang-lint`, `golang-modernize`,
`golang-stay-updated`; conditionally `golang-concurrency` and
`golang-security` when a change touches them.

## Samber Go suite — [samber/cc-skills-golang](https://github.com/samber/cc-skills-golang)

Preferred libraries **if** a contribution introduces the matching need (none
are current dependencies; adding one requires maintainer approval first):
`golang-samber-lo` ([samber/lo](https://github.com/samber/lo), generics
helpers), `golang-samber-oops` ([samber/oops](https://github.com/samber/oops),
error wrapping), `golang-samber-slog`
([samber/slog](https://github.com/samber/slog), structured logging),
`golang-samber-do` ([samber/do](https://github.com/samber/do), dependency
injection).
