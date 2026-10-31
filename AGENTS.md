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
protocol compatibility). Full human-facing detail lives in `README.md`. A
distilled, installable copy of this section ships as the `mcp-cli-usage`
skill: `/plugin marketplace add ConsulTent/mcpcli`.

### Contract

- Invocation: `mcp [flags] <command>`. Exactly one of `--command` or `--url`
  identifies the target (`auth logout` accepts either and never connects).
- Results go to stdout; decoration (progress lines, spinners) goes to stderr.
  Capture stdout only and stderr is irrelevant to parsing.
- `--json` makes every command print exactly one machine-readable JSON
  document (the SDK result object) on stdout and nothing else. Prefer it.
- Exit codes: `0` success, `1` any failure (bad flags, connect failure, tool
  error). Check the exit code before parsing. Two `jev` exceptions where a
  bad outcome is still data and exits `0`: escalated verdicts
  (`verdicts[].escalate`) and `jev check --probe --probe-on-auth-error
  skip` after a rejected provider key (`summary.auth_skipped > 0`).
- Error paths: failed one-shot runs print the error as plain text on stderr —
  stdout may be empty. The exceptions are a tool result with `isError`
  (the full JSON result still reaches stdout first, then the command exits 1)
  and every `jev check` outcome after a successful connect: the report
  document always prints on stdout first — in `--json` and human modes —
  including no-flavor, aborted-battery and probe-error cases. A connect
  failure still prints only the error (stdout may be empty).
- `--timeout` bounds each network operation (default 30s). Set it explicitly
  in scripts so a slow server cannot hang a step.
- Tool image/audio blocks render as one ASCII summary line each, never a
  base64 dump; resource blob contents print raw bytes unless `--outfile`
  diverts them. `--outfile` on `tools call`, `resources read`, and the REPL's
  `call`/`read` writes media content to files with MIME-derived extensions,
  printing `wrote …` notices to stderr in every mode.
- `--in-file [KEY=]PATH` (on `jev ask`/`screen`/`verify`, `tools call`,
  `prompts get`) reads a text input from a file (`-` for stdin, once per
  invocation; repeatable; 8 MiB cap; must be UTF-8 — invalid bytes are named
  with their offset). Values are ALWAYS raw strings, never JSON-decoded —
  unlike `--arg`. A bare PATH replaces the command's primary text (`jev ask`
  QUESTION, `jev screen` TEXT, mutually exclusive with the positional);
  `KEY=PATH` fills a named slot (`jev ask`: `context=` `evidence=`
  `priorities=`; `jev screen`: `purpose=`; `jev verify`: `claim=` `evidence=`
  — appending after flag values; `tools call`/`prompts get`: any argument
  key). Explicit flags beat files; sources load `-A` → `--in-file` → `--arg`.
  Each read prints `read <path> (N bytes)` to stderr in every mode. Prefer
  this over `$(cat file)` (argv caps, quoting, `/proc/<pid>/cmdline exposure)
  and over `-A -` + jq when the payload is one text value, not a JSON object.

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
| `-v`, `--version` | | print `mcp version vX.Y.Z` |

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

Jev judgment servers — flavor-agnostic typed questions; `--json` prints ONE
canonical document (`{flavor, tool, verdicts[], escalated?}`) for every
flavor, so parse that shape and never the per-server formats. `ask` infers
the primitive from the flags (`--opt` choice, `--levels` score, else noul):

```sh
mcp --command "npx -y @jkudish/jev-mcp" --json jev ask "is the build green?" --context "$CI_LOG"
# {"flavor":"jkudish","tool":"jev_noul","verdicts":[{"type":"noul","answer":0.99,"label":"likely"}]}
mcp --command "jev-use serve" --json jev ask "ship or hold?" --opt ship --opt hold
mcp --command "npx -y @jkudish/jev-mcp" --json jev verify --claim "tests pass" --evidence "$CI_LOG"
# verdicts[].answer is verified|contradicted|unsupported|unknown; label is the action (auto|review)
mcp --command "npx -y @jkudish/jev-mcp" --json jev check   # flavor + support matrix
```
(answers above are the live model's; they vary per call)

Text-sized inputs read straight from files with `--in-file` — no `$(cat
file)` (argv caps, quoting, `/proc/<pid>/cmdline exposure). Bare `PATH`
replaces the question/text; `KEY=PATH` fills a slot. The `read …` notice
goes to stderr, so piped stdout stays exactly the one canonical JSON
document:

```sh
mcp --command "npx -y @jkudish/jev-mcp" --json jev screen \
  --in-file internal/cli/testdata/gutenberg-sample.txt --purpose "code review"
# verdicts[].answer is the screen action (pass|review|block|skip)
```

Combined usage — files carry the big payloads, flags the short ones; jev
`evidence=`/`claim=` file values append after the flag values:

```sh
mcp --command "npx -y @jkudish/jev-mcp" --json jev ask "is the build green?" \
  --context "release checklist" --in-file evidence=ci.log
mcp --command "npx -y @jkudish/jev-mcp" --json jev verify \
  --in-file claim=claim.txt --in-file evidence=test-output.log
mcp --command "npx -y @jkudish/jev-mcp" --json jev verify \
  --claim "no regressions" --in-file evidence=pr.diff --in-file evidence=ci.log
mcp --url "$SRV" --json tools call jev_review \
  -A defaults.json --in-file diff=pr.diff --arg request="final check"
```

Sources load in order `-A` → `--in-file` → `--arg`, later overriding per key,
so `--arg request="final check"` beats any `request` inside `defaults.json`.

Self-test a Jev server (one minimal question per tool, per-probe timeouts;
skip-on-auth keeps a dead provider key from failing CI). Real capture,
2026-10-30, live jkudish server:

```sh
mcp --command "npx -y @jkudish/jev-mcp" --json jev check --probe --probe-on-auth-error skip --timeout 60s
# {"flavor":"jkudish","matched":["jev_verify","jev_screen","jev_noul",...],
#  "ops":[{"op":"noul","via":"jev_noul","ok":true},{"op":"score","ok":false,"note":"no ordered-levels tool; ..."},...],
#  "probes":[{"tool":"jev_noul","status":"ok"},...],
#  "summary":{"ok":12,"escalate":0,"skipped":0,"auth_skipped":0,"errors":0}}
# summary gate for scripts: jq -e '.summary.auth_skipped == 0 and .summary.errors == 0'
```

Diff-sized payloads (PR review, logs) never go through `--arg` — argv limits
and JSON re-typing break on exactly the large inputs. A single text value
(one diff, one log, one message) rides `--in-file KEY=PATH` (recipes above);
a composed multi-value JSON payload — several fields built from different
sources — rides `jq -n | -A -`, and stdin is consumed before any connection
opens:

```sh
jq -n --rawfile diff pr.diff --arg request "$PR_TITLE" \
  '{request: $request, diff: $diff, tests: "see the pull request checks"}' \
  | mcp --command "npx -y @jkudish/jev-mcp" --json --timeout 120s tools call jev_review -A -
```

(The example workflows' request+diff pair now prefers
`--in-file request=pr-request.txt --in-file diff=pr.diff`; the jq form stays
for payloads no single text slot can carry.)

The REPL (`mcp -i --url ...`) holds one session open for many operations;
the prompt goes to stderr and results to stdout, so piped stdout stays
parseable. REPL commands: `help`, `tools`, `call NAME [JSON-OBJ] [--outfile FILE]`,
`resources`, `read URI [--outfile FILE]`, `prompts`, `get NAME [JSON-OBJ]`, `info`,
`ping`, `exit`/`quit`. Each line runs under its own `--timeout` window.
