# Example workflows: code quality checks with the mcp CLI

Three copy-paste GitHub Actions workflows that review pull requests with the
[`mcp`](https://github.com/ConsulTent/mcpcli) CLI. Copy the file you want
into your repository under `.github/workflows/`, set the secrets it names,
and adjust the pinned versions to your taste.

| Workflow | Who reviews | Keys (your own) | When to choose |
|---|---|---|---|
| [`llm-pr-review.yml`](llm-pr-review.yml) | an LLM, every PR | `OPENROUTER_API_KEY` | You want the closest thing to Claude Code / Copilot review under your own prompt and model. Highest coverage, highest cost per PR. |
| [`jev-pr-review.yml`](jev-pr-review.yml) | TypeSafe Jev, every PR | `TYPESAFE_API_KEY` | You want fast, cheap, typed judgments (rubric scores, `safe_to_apply`, reason codes) and no LLM in the loop. Advisory scores, not prose. |
| [`hybrid-pr-review.yml`](hybrid-pr-review.yml) | Jev screens, LLM reviews only what Jev flags | both | TypeSafe's confidence-routing pattern: clean diffs never reach the LLM; flagged diffs get an LLM review focused by Jev's limiting rubrics. |

## Managed alternatives

If you'd rather not run anything yourself, the same jobs exist as products —
these examples trade that convenience for prompt/model control and a fixed
per-call cost:

- [`anthropics/claude-code-action`](https://github.com/anthropics/claude-code-action) — Claude Code as a GitHub Action (mention-triggered, auto-review recipes).
- Hosted `@claude review` — Claude reviews PRs on Anthropic infrastructure (Team/Enterprise plans).
- Copilot as a reviewer — `gh pr edit <number> --add-reviewer @copilot`. The
  request is scriptable from a workflow; the review itself always runs on
  GitHub's infrastructure.

## Setup common to all three

- **mcp ≥ v1.3.1** — each workflow installs it with
  `go install github.com/ConsulTent/mcpcli/cmd/mcp@v1.3.1`; bump the version
  as releases ship. v1.3.1 is the first release with `--in-file`.
- **Node** — preinstalled on GitHub-hosted runners; the MCP servers run via
  `npx`.
- **Secrets, never literals** — every key arrives through
  `${{ secrets.* }}`; none of these files contains a credential.
- **A dead key never fails the build.** The Jev workflows preflight with
  `mcp jev check --probe --probe-on-auth-error skip` and gate the review on
  the battery's JSON summary, so a rejected key (unpaid bill, rotation)
  posts a notice and exits green. The LLM workflow skips the same way when
  its key is absent.

## The two payload forms

Diff-sized payloads never go through `--arg`: argv length limits break on
exactly the large PRs review workflows exist for, and values that happen to
parse as JSON get silently re-typed. Pick the form by payload shape:

**Form 1 — single text values → `--in-file [KEY=]PATH`** (mcp ≥ v1.3.1). The
file is read as one raw string — never JSON-decoded — and argv carries only
the path, never the contents. This is what the workflows use for the diff,
the request text, and the LLM prompt:

```sh
mcp --command "npx -y @jkudish/jev-mcp@0.14.7" --json --timeout 120s \
  tools call jev_review --in-file request=pr-request.txt --in-file diff=pr.diff \
  --arg tests="see the pull request checks"
```

**Form 2 — whole JSON objects → `jq -n` piped into `-A -`.** When the payload
is a composed multi-field JSON object rather than one text value, build it
with jq and pipe it through stdin; stdin is consumed before any connection
opens:

```sh
jq -n --rawfile diff pr.diff --arg request "$REQUEST" '{request: $request, diff: $diff}' \
  | mcp --command "npx -y @jkudish/jev-mcp@0.14.7" --json --timeout 120s tools call jev_review -A -
```

Small scalars are fine as `--arg`. Every `mcp` call sets `--timeout`
explicitly and parses the single `--json` document on stdout.

## Pinning

- Actions are pinned to full-length commit SHAs (the same policy as the
  mcpcli repository's own workflows). Bump SHA and version comment together.
- The `npx` servers are version-pinned (`@jkudish/jev-mcp@0.14.7`,
  `openrouter-mcp@2.0.1`) so a workflow run is reproducible; bump
  deliberately. If you need stronger guarantees, pin npm packages by
  content hash the way the
  [mcp-e2e workflow](https://github.com/ConsulTent/mcpcli/blob/main/.github/workflows/mcp-e2e.yml)
  pins its fixtures.
