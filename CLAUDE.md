# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`mcpcli` is a Go module whose command is `mcp` (binary at `cmd/mcp`), an MCP client and inspector built on the official `github.com/modelcontextprotocol/go-sdk` (pinned at v1.8.0). The design spec (`docs/superpowers/specs/2026-10-05-mcpcli-design.md`) and the completed v1 task plan (`docs/IMPLEMENTATION_PLAN.md`) are local-only: they are gitignored along with `docs/upstream/`. The README is the user-facing contract for flags, output and exit codes, so update it whenever behavior changes. Contributions are AI-assisted and follow a fixed workflow — read `CONTRIBUTING.md` before making any change.

## Commands

Requires Go ≥ 1.27.1.

```
go build ./...
go run ./cmd/mcp --url https://example.com/mcp info
go vet ./...
go test ./...
go test ./internal/cli/ -run TestToolsCallE2E -count=1     # single test
go test ./internal/cli/ -skip TestCompatUnreachable        # fast iteration
golangci-lint run                                          # no repo config; default linters
```

`TestCompatUnreachable` takes about 3.5 minutes because its 7 connects each wait out the 30s default `--timeout`, so skip it while iterating and run the full suite before committing. The lint baseline is not clean: `golangci-lint run` (no repo config, default linters) reports ~27 pre-existing findings (26 errcheck + a staticcheck SA1019 on the deprecated `ServerCapabilities.Logging`) — do not block on them or fix them wholesale, but do not add new ones. `.github/workflows/` holds: `ci.yml` (vet + tests on PRs and main), `mcp-e2e.yml` (reusable MCP-server e2e + weekly + a per-merge repo-built self-test job `e2e-main` on main pushes; its steps are inlined per job, not shared via local composite actions — `./` action refs in a reusable workflow resolve against the caller's workspace), `release.yml` (monthly/CVE patch rollups), and `release-build.yml` (builds+attests+uploads release binaries on release; sync-homebrew-tap opens+merges the homebrew-tap bump PR).

**Releases:** publishing a GitHub release runs `.github/workflows/release-build.yml`: one Linux job cross-compiles the six `mcp-<os>-<arch>` binaries (`CGO_ENABLED=0 -trimpath`, Go from `go.mod`), attests them with `actions/attest` (GitHub artifact attestations; OIDC subject auto-derived from the repo), and uploads them to the release. Consumers verify with `gh attestation verify <binary> --repo ConsulTent/mcpcli`. `workflow_dispatch` runs the same job as a build+attest dress rehearsal (pass `-f tag=<release tag>` — the input is required; the sync job then no-ops if the tap is already current; no upload; build-only under act, which cannot mint OIDC tokens). All actions are pinned to full-length commit SHAs with `# vX.Y.Z` comments — bump SHA and comment together. The binary's version comes from `internal/version`: the `Version` constant is the baseline, and `version.Current()` prefers the module stamp from `debug.ReadBuildInfo()` (release tags win) — there are no ldflags. Every substantive change bumps the minor version; see the Versioning section of `CONTRIBUTING.md`.

## Architecture

The flow runs `cmd/mcp/main.go` → `internal/cli` (cobra tree) → `runWithConnection` (`internal/cli/run.go`) → auth provider `Build` → `connection.Open` → the command's `fn(ctx, session)`, which calls one SDK `ClientSession` method and hands the result to `internal/format`.

- **Global flags** are bound into one `GlobalFlags` struct that is stored on the root command's context. Subcommands get it with `FlagsFrom(cmd)`. `PersistentPreRunE` validates `--auth` against the registry, rejects `-i` combined with a subcommand, and sets the output mode.
- **`runWithConnection`** is the one path to a server. It enforces exactly one of `--command`/`--url` before any network access. One `--timeout` window covers the OAuth wait, the connect and `fn`. Errors propagate unchanged, and any error becomes exit 1.
- **Commands that call `connection.Open` directly:**
  - `compat` opens its own pinned, always-unauthenticated probe sessions.
  - `jev check` opens one auth-honoring session and carves per-probe `--timeout` windows from the command context (the battery's state machine — statuses `ok|escalate|skipped|auth_skipped|error`, summary counts that always sum to the row count, doc-before-exit — is pinned in `internal/cli/jev_test.go`).
  - `auth login` forces the `oauth` provider and pings.
  - `auth logout` is strictly offline and never connects.
- **REPL (`-i`)** runs inside `runWithConnection`, but it uses `context.WithoutCancel` so the session can outlive the connect window. Each REPL line gets its own `--timeout` window.
- **`internal/connection`** builds the transport and the client. stdio uses `shellquote.Split` and `mcp.CommandTransport`, and a failed connect is wrapped with the child's captured stderr (bounded at 4 KiB). HTTP uses `mcp.StreamableClientTransport` with the auth handler. stdio rejects any auth handler.
- **Protocol versions:**
  - `KnownVersions` in `versions.go` mirrors the SDK's `mcp.SupportedProtocolVersions()`. Update it and the README together when bumping go-sdk.
  - A known date resolves to itself even when the SDK can't speak it. `compat` depends on that to report "not-speakeable".
  - `draft` means the newest version the SDK supports.

### Auth is a registry, not a mode switch

`internal/auth/provider.go` holds the `Provider` interface (`Name`, `Build`) and the registry. The CLI must never switch on auth mode names. Validation, error messages and dispatch all come from the registry.

- To add a provider, write one new file that implements `Provider`, then register it in `RegisterBuiltins` (called from `cli`'s `init`).
- If `Build` returns `(nil, nil, nil)`, the transport sends no Authorization header.
- The SDK contract is: `TokenSource` sets the header, and on a 401/403 the SDK calls `Authorize` and retries once.
- OAuth must never launch a browser. It either prints the URL and waits on a `127.0.0.1` loopback receiver, or uses `--auth-flow manual` to read a pasted code from stdin.
- Tokens are cached in `TokenDir/tokens.json`, keyed by SHA-256 of the target string.

### Jev is an adapter slice, not a registry

`internal/jev` maps one canonical question model (TypeSafe primitives: `OpKindNoul/Choice/Score` + `Verify/Screen/Gate`) onto the known Jev MCP servers — they share no tool naming, and `jev_gate` means different things on different flavors, so ops map by semantics. `var flavors = []Adapter{jkudish{}, jevUse{}, legostin{}}` + `AdapterFor(tools)` (first Detect hit wins; auth needs runtime lookup by name, jev does not — that is why there is no Register here).

- To add a flavor, write one new file implementing `Adapter` (`Name`, `Detect`, `Vocabulary`, `Supports`, `Build`, `Parse`, `Probes`) and add it to the slice; no CLI change.
- `Parse` is the single type-enforcement point for untrusted tool-result JSON: per-op `Answer` typing, required-shape checks, `StructuredContent`-else-text precedence. A nil or wrong-typed answer is a parse error, never a downstream panic.
- `IsAuthError` matches provider rejections by text (token-bounded 401/402/403, "invalid api key", payment/billing); its pattern list is deliberately separate from `connection`'s connect-time heuristic — cross-referenced by comment, not shared.
- The canonical `--json` document (`{flavor, tool, verdicts, escalated?}`) is the merge contract: one shape for every flavor; raw per-flavor output stays reachable via `tools call`.

### Output contract

- Results go to stdout. Decoration (steps, spinners, status lines) goes to stderr.
- `ui.Current` is a package global (`Rich`/`Plain`/`JSON`) set once per run.
- `--json`: each command emits one `format.EmitJSON` document of the SDK result struct, and the ui layer writes nothing.
- `--plain`: output must contain zero non-ASCII and zero ANSI-escape bytes. Tests assert this.
- Image/audio content blocks render as one ASCII summary line each (`[image/png content, N bytes]`); `--outfile` (tools call, resources read, REPL call/read) writes image/audio blocks and resource blobs to files with MIME-derived extensions and prints `wrote` notices to stderr in every mode.
- `internal/format` is pure formatting with no mode logic, colors or emoji. Callers choose between a formatter and `EmitJSON`.
- A tool result with `isError` returns `format.ErrToolError` (exit 1). In `--json` mode the full result still reaches stdout first.
- The `--client-secret` warning is written directly to stderr in every mode, `--json` included. Keep it that way.
- `internal/ui` reproduces go-gitmoji-cli's spinner look using bubbletea, bubbles and lipgloss directly. go-gitmoji-cli is deliberately not a dependency, because its `pkg/ui` hard-wires stdout and `os.Exit`.
- In the spinner, `fn`'s outcome is authoritative. A bubbletea failure never turns a successful `fn` into an error.

## Testing conventions

- **No test seams for transports.** HTTP tests use `httptest` with `mcp.NewStreamableHTTPHandler` wrapped around an in-process `*mcp.Server` (`newTestMCPServer` in `internal/cli/run_test.go`). stdio tests use the `TestHelperProcess` subprocess idiom in `internal/connection/connection_test.go`. OAuth tests use `httptest` fake authorization servers. Jev tests use fixture servers registering each flavor's tools with canned results (`addJkudishFixture`/`addJevUseFixture`/`addLegostinFixture` in `internal/cli/jev_test.go`), including auth-rejecting variants in both error shapes (CallTool error and isError result).
- CLI end-to-end tests go through `runRoot(t, args...)`, which captures stdout, stderr and ui decoration.
- **Don't use `t.Parallel()` in `cli`, `ui` or `auth` tests.** `ui.Current`, the spinner writer (`ui.SetSpinnerOutput`) and `auth.TokenDir` are package globals. Tests point `auth.TokenDir` at `t.TempDir()` and restore it afterward.
- Credentials in tests are dummies derived per test and set with `t.Setenv`. Never put a usable-looking credential literal in source, examples or tests.
- Pre-connect validation errors are golden strings pinned by tests (`TestJevArgumentErrors`); when a message changes, change its test in the same commit. `examples/workflows/` must stay actionlint-clean — ci.yml's `examples` job enforces it.

## Repo conventions

- `.gitattributes` forces LF for every text file. A CRLF checkout breaks the source-scan and golden tests, and this repo lives on a Windows-mounted drive, so leave it in place.
- `WORKING_STATUS.json` (gitignored, never commit it) is the shared work-status ledger for agents and subagents. When an agent starts a planned task, it sets the task's status to `in_progress` and records itself as owner. When it finishes, it sets `done` or `blocked` and adds notes.
- Commit messages use conventional prefixes (`feat:`, `fix:`, `docs:`, `chore:`).
