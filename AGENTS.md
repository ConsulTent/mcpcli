# AGENTS.md

Workspace instructions for ZCode agents working in this repository.

mcpcli contributions are AI-assisted and follow a fixed workflow. Before
making any change, read — in this order:

1. `README.md` — the user-facing contract (flags, output modes, exit codes).
2. `CLAUDE.md` — architecture, conventions, and testing rules.
3. `CONTRIBUTING.md` — the required AI contribution workflow and the skills
   to load (superpowers planning/execution, $ponytail review, Go and samber
   Go skills).

While executing a planned task, keep `WORKING_STATUS.json` (repo root,
gitignored — never commit it) updated: `in_progress` + owner on start,
`done`/`blocked` + notes on finish.

## Using the mcp CLI (for AI agents)

This section is the programmatic contract for agents that shell out to the
`mcp` binary (for example to inspect an MCP server, call its tools, or check
protocol compatibility). Full human-facing detail lives in `README.md`.

### Contract

- Invocation: `mcp [flags] <command>`. Exactly one of `--command` or `--url`
  identifies the target (`auth logout` accepts either and never connects).
- Results go to stdout; decoration (progress lines, spinners) goes to stderr.
  Capture stdout only and stderr is irrelevant to parsing.
- `--json` makes every command print exactly one machine-readable JSON
  document (the SDK result object) on stdout and nothing else. Prefer it.
- Exit codes: `0` success, `1` any failure (bad flags, connect failure, tool
  error). Check the exit code before parsing.
- Error paths: failed one-shot runs print the error as plain text on stderr —
  stdout may be empty. The one exception is a tool result with `isError`:
  the full JSON result still reaches stdout first, then the command exits 1.
- `--timeout` bounds each network operation (default 30s). Set it explicitly
  in scripts so a slow server cannot hang a step.
- Tool image/audio blocks render as one ASCII summary line each, never a
  base64 dump; resource blob contents print raw bytes unless `--outfile`
  diverts them. `--outfile` on `tools call`, `resources read`, and the REPL's
  `call`/`read` writes media content to files with MIME-derived extensions,
  printing `wrote …` notices to stderr in every mode.

### Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--command` | | stdio server to launch, e.g. `"npx -y @modelcontextprotocol/server-filesystem /tmp"` |
| `--url` | | streamable HTTP endpoint of the MCP server |
| `--auth` | `none` | `none`, `bearer`, `client-credentials`, `oauth` (HTTP only; stdio rejects it) |
| `--auth-flow` | `loopback` | OAuth flow: `loopback` or `manual`; only used with `--auth oauth`; `manual` reads a pasted code from stdin (headless-safe) |
| `--protocol` | `auto` | `auto`, `draft`, or a date such as `2025-06-18` |
| `--client-id` | | OAuth client ID (env `MCPCLI_CLIENT_ID`) |
| `--client-secret` | | OAuth client secret / bearer token (env `MCPCLI_CLIENT_SECRET`); flags win over env |
| `--timeout` | `30s` | timeout for network operations |
| `--json` | `false` | one JSON document on stdout, no decoration |
| `--plain` | `false` | byte-ASCII output (no emoji/ANSI) |
| `-i`, `--interactive` | `false` | line-based REPL (no subcommand) |
| `--version` | | print `mcp version vX.Y.Z` |

### Commands (each example is the recipe)

Probe connectivity and see what the server offers:

```sh
mcp --url https://mcp.svelte.dev/mcp --json ping    # {"ok":true,"latency_ms":231,"via":"tools/list"}
mcp --url https://mcp.svelte.dev/mcp --json info    # name, version, protocolVersion, capabilities
```

`ping` verifies liveness with the best available method: plain ping, or — on
2026-07-28 servers that removed it — the first declared capability list call,
named in the JSON `"via"` field.

Discover tools, then call one (arguments JSON-decode when they parse:
`count=3` is the number 3, `x={"a":1}` an object; repeated keys: last wins;
`-A file` loads a JSON object, `-` reads stdin):

```sh
mcp --url "$SRV" --json tools list
mcp --url "$SRV" --json tools call resolve-library-id --arg query=svelte --arg libraryName=svelte
mcp --url "$SRV" --json tools call generate-logo --arg prompt="red circle" --outfile logo.png
```

A tool error (`isError: true`) still prints the full JSON result, exit 1.

Resources and prompts (prompt arguments are always raw strings):

```sh
mcp --url "$SRV" --json resources list
mcp --url "$SRV" --json resources read svelte://ai/overview.md
mcp --url "$SRV" --json resources read config://badge.png --outfile badge.png
mcp --url "$SRV" --json prompts list
mcp --url "$SRV" --json prompts get svelte-task --arg task="fix my component"
```

Spec-check an endpoint — one row per released protocol version plus `draft`,
and the negotiated native version (rejections are data, not failures):

```sh
mcp --url "$SRV" --json compat
# {"native":"2026-07-28","results":[{"version":"2025-11-25","status":"accepted","detail":"requested 2025-11-25; server negotiated 2025-06-18"}, ...]}
```

Credentials and headless OAuth (never launch a browser; `manual` prints the
authorization URL and reads the pasted code from stdin; tokens cache under
`os.UserConfigDir()/.mcpcli`):

```sh
export MCPCLI_CLIENT_ID=... MCPCLI_CLIENT_SECRET=...
mcp auth login --url "$SRV" --auth-flow manual --timeout 60s
mcp auth logout --url "$SRV"
```

stdio servers (local tools) — pass the whole command line as one string:

```sh
mcp --command "npx -y @modelcontextprotocol/server-everything" --json tools list
```

The REPL (`mcp -i --url ...`) holds one session open for many operations;
the prompt goes to stderr and results to stdout, so piped stdout stays
parseable. REPL commands: `help`, `tools`, `call NAME [JSON-OBJ] [--outfile FILE]`,
`resources`, `read URI [--outfile FILE]`, `prompts`, `get NAME [JSON-OBJ]`, `info`,
`ping`, `exit`/`quit`. Each line runs under its own `--timeout` window.
