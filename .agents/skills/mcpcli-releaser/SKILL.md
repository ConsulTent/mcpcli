---
name: mcpcli-releaser
description: Cut and ship a new mcpcli release (github.com/ConsulTent/mcpcli) — minor preferred, major on breaking changes, patch only when the user names one. Use whenever the user asks to release mcpcli, cut or ship a version (vX.Y.Z), bump and publish, or "push the new version". Covers gates, the clean-stamp check, tag + GitHub Release, attested binaries, and the hands-free Homebrew tap sync.
---

# mcpcli releaser

One proven loop, verified end to end on v1.3.1, v1.3.2 and v1.3.3. Follow it
in order; every step exists because skipping it once cost a re-tag, a red
run, or a premature merge.

## 0. Confirm the version — never guess it

The policy in CONTRIBUTING.md says substantive → minor; the maintainer has
twice overridden a planned minor to a patch at the last moment. Always state
the intended version and get agreement before touching anything:
minor preferred (default), major for breaking changes, patch only if the
user names it. If `internal/version/version.go` already carries the target
(the feature round bumped it), skip the bump commit.

## 1. Pre-flight (~2 min)

```sh
git fetch origin && git status -sb        # main...origin/main must be even
grep -n 'const Version' internal/version/version.go
```

- Someone else's commit lands mid-round regularly — if remote moved, stop
  and integrate first (the local branch is behind, never force).
- Sanity-check no other release is in flight: `gh run list -R ConsulTent/mcpcli
  --workflow=release-build.yml --limit 1` (the monthly `release.yml` cron
  can also cut releases; don't collide).

## 2. Gates (~5 min; skip any the feature round just ran, but say so)

```sh
go vet ./... && go test ./... -count=1 -skip TestCompatUnreachable
golangci-lint run ./...        # must equal the ~27-finding baseline; zero new
go mod tidy && git diff --exit-code go.mod go.sum
"$(go env GOPATH)/bin/actionlint" .github/workflows/*.yml   # if workflows changed
"$(go env GOPATH)/bin/govulncheck" ./...    # posture: stdlib-only findings = toolchain; no called-vuln deltas
```

If the version constant needs bumping: edit `internal/version/version.go`,
commit `chore: version v<X.Y.Z>`, `git push origin main` (branch protection
is bypassed by the configured PAT — the "Bypassed rule violations" remote
message is expected, not an error).

## 3. Clean-stamp check — the #1 release-killer

```sh
go build -o "$TMP/mcp-rel.exe" ./cmd/mcp && "$TMP/mcp-rel.exe" --version
```

Must print EXACTLY `mcp version v<X.Y.Z>`. Anything else means:
- `+dirty` → untracked scratch (build dirs, SDD workspaces). Add the path
  to `.gitignore`, commit, push — and note an UNCOMMITTED .gitignore edit
  still stamps dirty; commit first.
- a pseudo-version (`v<X.Y.Z>-0.2026…-<hash>`) on a pre-tag build is normal;
  the tag checkout in CI stamps clean. What matters is no `+dirty`.

## 4. Tag + release

```sh
git tag v<X.Y.Z> && git push origin v<X.Y.Z>
gh release create v<X.Y.Z> --title "mcp v<X.Y.Z>" --notes "<one paragraph per shipped feature, concrete>"
```

Release notes style: "New: `--flag` does X — one sentence of what, one of
why." Copy the shape of the v1.3.3 notes.

## 5. Watch the pipeline (~5 min)

```sh
run=$(gh run list -R ConsulTent/mcpcli --workflow=release-build.yml --limit 1 --json databaseId --jq '.[0].databaseId')
gh run watch "$run" -R ConsulTent/mcpcli --exit-status
```

Two jobs must go green: `build` (cross-compiles 6 binaries, attests them,
uploads) and `sync-homebrew-tap` (opens the tap bump PR from release-API
digests, waits for test-bot on both platforms via the watch-retry, squash-
merges, deletes its branch).

**Never merge the tap PR manually while checks are pending.** `gh pr merge`
does not wait (the tap has no required checks) — that mistake was made once;
the automation itself refuses CI-less merges and that is correct. If
`sync-homebrew-tap` is red, the PR is intentionally left open: read the log,
fix forward, re-dispatch `release-build.yml -f tag=v<X.Y.Z>` (the job is
idempotent — it no-ops with "tap already at <tag>" when done).

## 6. Verify on the real artifacts

```sh
gh api repos/ConsulTent/mcpcli/releases/tags/v<X.Y.Z> --jq '.assets | length'   # 6
cd "$TMP" && gh release download v<X.Y.Z> -R ConsulTent/mcpcli --pattern "mcp-windows-amd64.exe" --clobber
gh attestation verify mcp-windows-amd64.exe --repo ConsulTent/mcpcli   # exit 0
./mcp-windows-amd64.exe --version                                       # exactly v<X.Y.Z>
gh api repos/ConsulTent/homebrew-tap/contents/Formula/mcpcli.rb --jq .content \
  | base64 -d | grep -cE "v<X.Y.Z>"                                     # 4
gh pr list -R ConsulTent/homebrew-tap --state open --json number --jq length  # 0
```

The user's own Renovate on the tap opens a duplicate bump PR per release;
it autocloses behind ours. If both ever stay open, ours wins, Renovate's
autocloses — do not merge both.

## 7. Close the books

- Append the release entry to `WORKING_STATUS.json` (gitignored ledger in
  the main checkout) with run id + verification evidence.
- Update the session memory: version, run id, anything the round taught.

## Failure playbook

- Dirty stamp at step 3 → gitignore the scratch, commit, RE-TAG (delete
  local+remote tag only if no release was published yet; if published, cut
  the next patch instead of moving a live tag).
- Attestation step failed in release-build → it gates upload; the run is red
  and nothing published; fix and re-dispatch.
- `no checks reported` on the tap PR → the watch-retry already tolerates
  ~10 min of registration latency; if it still times out the PR stays open
  — inspect test-bot on the tap, then re-dispatch the sync (idempotent).
