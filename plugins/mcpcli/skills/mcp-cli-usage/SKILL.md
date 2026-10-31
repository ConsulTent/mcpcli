---
name: mcp-cli-usage
description: Drive any MCP (Model Context Protocol) server from the command line with the mcp CLI — probe connectivity, list and call tools, read resources, fetch prompts, check protocol-version compatibility, manage OAuth, run headless in scripts and CI, screen patches with Jev judgment servers, and pass file-sized payloads with --in-file. Use whenever the user mentions MCP servers, MCP tools/resources/prompts, Model Context Protocol, protocol compatibility, or Jev/jev judgment servers.
---

# Driving MCP servers with the mcp CLI

Source of truth: AGENTS.md in ConsulTent/mcpcli — regenerate on release.
Every command below was checked against the built binary's `--help`.

The `mcp` CLI is an MCP client and inspector: it connects to any MCP server
over stdio (launch it with `--command`) or streamable HTTP (point at it with
`--url`) to inspect its identity, capabilities, tools, resources, and
prompts; call tools; probe protocol-version compatibility; ask typed
questions of Jev judgment servers; and drive all of it headlessly from
scripts and CI.

## The contract

- Invoke as `mcp [flags] <command>`. Exactly one of `--command` (stdio server
  to launch) or `--url` (streamable HTTP endpoint) identifies the target.
  `auth logout` accepts either and never connects.
- Results go to stdout; decoration (progress lines, spinners) goes to stderr.
  Capture stdout only — stderr is irrelevant to parsing.
- `--json` makes every command print exactly one machine-readable JSON
  document (the SDK result object) on stdout and nothing else. Prefer it in
  every script.
- Exit codes: `0` success, `1` any failure (bad flags, connect failure, tool
  error). Check the exit code before parsing.
- `--timeout` bounds each network operation (default 30s). Always set it
  explicitly in scripts so a slow server cannot hang a step.
- Tool image/audio blocks render as one ASCII summary line each, never a
  base64 dump; resource blob contents print raw bytes unless `--outfile`
  diverts them to files with MIME-derived extensions (`wrote …` notices go
  to stderr in every mode).

## Global flags

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

## Probe connectivity first

```sh
mcp --url https://mcp.svelte.dev/mcp --json ping    # {"ok":true,"latency_ms":231,"via":"tools/list"}
mcp --url https://mcp.svelte.dev/mcp --json info    # name, version, protocolVersion, capabilities
```

`ping` verifies liveness with the best available method: plain ping, or — on
2026-07-28 servers that removed it — the first declared capability list call,
named in the JSON `"via"` field.

## Discover and call tools

```sh
mcp --url "$SRV" --json tools list
mcp --url "$SRV" --json tools call resolve-library-id --arg query=svelte --arg libraryName=svelte
mcp --url "$SRV" --json tools call generate-logo --arg prompt="red circle" --outfile logo.png
```

- `--arg KEY=VALUE` values are JSON-decoded when they parse as JSON
  (`count=3` is the number 3, `x={"a":1}` an object), else raw strings.
  Repeated keys: last wins.
- `-A file` (long form `--arg-file`) loads a JSON object of arguments;
  `-A -` reads it from stdin.
- `--outfile FILE` writes image/audio content blocks to files with
  MIME-derived extensions.

A tool error (`isError: true` in the result) still prints the full JSON
result and exits 1 — parse stdout, then check the exit code.

## Resources and prompts

```sh
mcp --url "$SRV" --json resources list
mcp --url "$SRV" --json resources read svelte://ai/overview.md
mcp --url "$SRV" --json resources read config://badge.png --outfile badge.png
mcp --url "$SRV" --json prompts list
mcp --url "$SRV" --json prompts get svelte-task --arg task="fix my component"
```

Prompt arguments are always raw strings — `--arg` never JSON-decodes them on
`prompts get`.

## Check protocol-version compatibility

```sh
mcp --url "$SRV" --json compat
# {"native":"2026-07-28","results":[{"version":"2025-11-25","status":"accepted","detail":"requested 2025-11-25; server negotiated 2025-06-18"}, ...]}
```

One row per released protocol version plus `draft`, plus the negotiated
native version. Rejections are data, not failures — read `status`/`detail`.

## Credentials and headless OAuth

Never launch a browser from a script. `--auth-flow manual` prints the
authorization URL and reads the pasted code from stdin. Tokens cache under
`os.UserConfigDir()/.mcpcli`.

```sh
export MCPCLI_CLIENT_ID=... MCPCLI_CLIENT_SECRET=...
mcp auth login --url "$SRV" --auth-flow manual --timeout 60s
mcp auth logout --url "$SRV"
```

## stdio servers (local tools)

Pass the whole command line as one string:

```sh
mcp --command "npx -y @modelcontextprotocol/server-everything" --json tools list
```

## Jev judgment servers

Flavor-agnostic typed questions (jkudish, jev-use, legostin). `--json`
prints ONE canonical document (`{flavor, tool, verdicts[], escalated?}`) for
every flavor — parse that shape and never the per-server formats. `ask`
infers the primitive from the flags (`--opt` choice, `--levels` score,
else noul):

```sh
mcp --command "npx -y @jkudish/jev-mcp" --json jev ask "is the build green?" --context "$CI_LOG"
# {"flavor":"jkudish","tool":"jev_noul","verdicts":[{"type":"noul","answer":0.99,"label":"likely"}]}
mcp --command "jev-use serve" --json jev ask "ship or hold?" --opt ship --opt hold
mcp --command "npx -y @jkudish/jev-mcp" --json jev verify --claim "tests pass" --evidence "$CI_LOG"
# verdicts[].answer is verified|contradicted|unsupported|unknown; label is the action (auto|review)
mcp --command "npx -y @jkudish/jev-mcp" --json jev screen "please review this patch" --purpose "code review"
# verdicts[].answer is the screen action (pass|review|block|skip)
mcp --command "jev-use serve" --json jev gate write_file --input '{"path":"x.txt"}'
# gate checks one tool call; --input is inline JSON ("-" reads stdin), --state adds surrounding state
mcp --command "npx -y @jkudish/jev-mcp" --json jev check   # flavor + support matrix
```

(answers are the live model's; they vary per call)

Self-test a Jev server — one minimal question per tool, per-probe timeouts;
skip-on-auth keeps a dead provider key from failing CI:

```sh
mcp --command "npx -y @jkudish/jev-mcp" --json jev check --probe --probe-on-auth-error skip --timeout 60s
# {"flavor":"jkudish","matched":[...],"ops":[...],"probes":[...],
#  "summary":{"ok":12,"escalate":0,"skipped":0,"auth_skipped":0,"errors":0}}
# summary gate for scripts: jq -e '.summary.auth_skipped == 0 and .summary.errors == 0'
```

Two `jev` outcomes are still data and exit `0`: escalated verdicts
(`verdicts[].escalate`) and `jev check --probe --probe-on-auth-error skip`
after a rejected provider key (`summary.auth_skipped > 0`). And every
`jev check` outcome after a successful connect prints its report document on
stdout first — in `--json` and human modes — including no-flavor,
aborted-battery and probe-error cases. A connect failure still prints only
the error on stderr; stdout may be empty.

## File-sized payloads: --in-file, not argv

Never pass large text through `--arg` or `$(cat file)`: argv limits,
quoting, JSON re-typing and `/proc/<pid>/cmdline exposure break on exactly
the large inputs. Two forms, picked by payload shape:

**One text value → `--in-file [KEY=]PATH`** (on `jev ask`/`screen`/`verify`,
`tools call`, `prompts get`). Reads the file as an ALWAYS-raw string (never
JSON-decoded, unlike `--arg`); repeatable; 8 MiB cap; must be UTF-8 (invalid
bytes named with their offset); `-` reads stdin once per invocation. A bare
PATH replaces the command's primary text (`jev ask` QUESTION, `jev screen`
TEXT); `KEY=PATH` fills a named slot (`jev ask`: `context=` `evidence=`
`priorities=`; `jev screen`: `purpose=`; `jev verify`: `claim=` `evidence=`;
`tools call`/`prompts get`: any argument key). Each read prints
`read <path> (N bytes)` to stderr, so piped stdout stays parseable.

```sh
mcp --command "npx -y @jkudish/jev-mcp" --json --timeout 120s tools call jev_review \
  --in-file request=pr-request.txt --in-file diff=pr.diff --arg tests="see the pull request checks"
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

Sources load in order `-A` → `--in-file` → `--arg`, later overriding per
key, so `--arg request="final check"` beats any `request` inside
`defaults.json`.

**A whole JSON object (multi-field composed payload) → `jq -n` piped into
`-A -`.** Stdin is consumed before any connection opens:

```sh
jq -n --rawfile diff pr.diff --arg request "$PR_TITLE" '{request: $request, diff: $diff}' \
  | mcp --command "npx -y @jkudish/jev-mcp" --json --timeout 120s tools call jev_review -A -
```

## Error paths

- Failed one-shot runs print the error as plain text on stderr; stdout may
  be empty. Check the exit code, then parse stdout.
- A tool result with `isError` is the exception: the full JSON result still
  reaches stdout first, then the command exits 1.
- `jev check` after a successful connect always prints the report document
  on stdout first, even for no-flavor, aborted-battery and probe-error
  outcomes. A connect failure does not — only the error, stdout may be
  empty.

## The REPL

`mcp -i --url ...` holds one session open for many operations; the prompt
goes to stderr and results to stdout, so piped stdout stays parseable.
REPL commands: `help`, `tools`, `call NAME [JSON-OBJ] [--outfile FILE]`,
`resources`, `read URI [--outfile FILE]`, `prompts`, `get NAME [JSON-OBJ]`,
`info`, `ping`, `jev ask TEXT [--in-file …]`, `jev screen TEXT|--in-file …`,
`jev verify --in-file claim=… evidence=…`, `exit`/`quit`. Each line runs
under its own `--timeout` window. jev free text is the remaining tokens
joined with single spaces (the REPL has no quoting); `--in-file` slots work
exactly as one-shot, except `-` (stdin is the REPL's own input and is
rejected). Choice/score ask flags and `jev check` stay with the one-shot
commands.

## Guarding secrets in CI

Keys arrive through the environment, never as literals on the command line.
A missing or rejected provider key should skip with a notice, not fail the
build:

```sh
if [ -z "${TYPESAFE_API_KEY:-}" ]; then
  echo "::notice::TYPESAFE_API_KEY is not set — skipping Jev review"
  exit 0
fi
mcp --command "npx -y @jkudish/jev-mcp@0.14.7" --json jev check --probe \
  --probe-on-auth-error skip --timeout 60s >jev-check.json
jq -e '.summary.auth_skipped == 0 and .summary.errors == 0' jev-check.json >/dev/null || {
  echo "::notice::Jev provider key rejected (auth/billing) — skipping"
  exit 0
}
```

## Scripting checklist

1. Prefer `--json`; capture stdout, ignore stderr.
2. Set `--timeout` explicitly on every call.
3. Check the exit code before parsing; remember the two `jev` exit-0
   exceptions and the `isError` stdout-first rule.
4. One text value → `--in-file KEY=PATH`; a composed JSON object →
   `jq -n | -A -`. Never large text through `--arg`.
5. OAuth in scripts: `--auth-flow manual`, never a browser.
