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

`TestCompatUnreachable` takes about 3.5 minutes because its 7 connects each wait out the 30s default `--timeout`, so skip it while iterating and run the full suite before committing. The lint baseline is not clean: `golangci-lint run` (no repo config, default linters) reports ~27 pre-existing findings (26 errcheck + a staticcheck SA1019 on the deprecated `ServerCapabilities.Logging`) — do not block on them or fix them wholesale, but do not add new ones. `.github/workflows/` holds two workflows: the SLSA release builder and Renovate, which opens daily dependency PRs (`renovate.json`, token from the `GH_PAT` secret). Nothing runs tests in CI, so run the suite locally on Renovate PRs. Keep Renovate on `config:recommended`: digest-pinning presets (`config:best-practices`, `helpers:pinGitHubActionDigests`) break SLSA, which requires the builder at `@vX.Y.Z`.

**Releases:** publishing a GitHub release runs `.github/workflows/go-ossf-slsa3-publish.yml`. It runs one SLSA3 Go builder job per `.slsa-goreleaser/<os>-<arch>.yml`, and the Go version comes from `go.mod`. The binary's version comes from `internal/version`: the `Version` constant is the baseline, and `version.Current()` prefers the module stamp from `debug.ReadBuildInfo()` (release tags win) — there are no ldflags. Every substantive change bumps the minor version; see the Versioning section of `CONTRIBUTING.md`. The builder runs `go mod vendor` before it builds, so `vendor/` must stay in `.gitignore`. If it isn't ignored, release binaries report `vX.Y.Z+dirty`.

## Architecture

The flow runs `cmd/mcp/main.go` → `internal/cli` (cobra tree) → `runWithConnection` (`internal/cli/run.go`) → auth provider `Build` → `connection.Open` → the command's `fn(ctx, session)`, which calls one SDK `ClientSession` method and hands the result to `internal/format`.

- **Global flags** are bound into one `GlobalFlags` struct that is stored on the root command's context. Subcommands get it with `FlagsFrom(cmd)`. `PersistentPreRunE` validates `--auth` against the registry, rejects `-i` combined with a subcommand, and sets the output mode.
- **`runWithConnection`** is the one path to a server. It enforces exactly one of `--command`/`--url` before any network access. One `--timeout` window covers the OAuth wait, the connect and `fn`. Errors propagate unchanged, and any error becomes exit 1.
- **Commands that call `connection.Open` directly:**
  - `compat` opens its own pinned, always-unauthenticated probe sessions.
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

- **No test seams for transports.** HTTP tests use `httptest` with `mcp.NewStreamableHTTPHandler` wrapped around an in-process `*mcp.Server` (`newTestMCPServer` in `internal/cli/run_test.go`). stdio tests use the `TestHelperProcess` subprocess idiom in `internal/connection/connection_test.go`. OAuth tests use `httptest` fake authorization servers.
- CLI end-to-end tests go through `runRoot(t, args...)`, which captures stdout, stderr and ui decoration.
- **Don't use `t.Parallel()` in `cli`, `ui` or `auth` tests.** `ui.Current`, the spinner writer (`ui.SetSpinnerOutput`) and `auth.TokenDir` are package globals. Tests point `auth.TokenDir` at `t.TempDir()` and restore it afterward.
- Credentials in tests are dummies derived per test and set with `t.Setenv`. Never put a usable-looking credential literal in source, examples or tests.

## Repo conventions

- `.gitattributes` forces LF for every text file. A CRLF checkout breaks the source-scan and golden tests, and this repo lives on a Windows-mounted drive, so leave it in place.
- `WORKING_STATUS.json` (gitignored, never commit it) is the shared work-status ledger for agents and subagents. When an agent starts a planned task, it sets the task's status to `in_progress` and records itself as owner. When it finishes, it sets `done` or `blocked` and adds notes.
- Commit messages use conventional prefixes (`feat:`, `fix:`, `docs:`, `chore:`).
