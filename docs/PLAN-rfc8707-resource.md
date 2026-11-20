# Plan: client-credentials vs resource-indicator MCP servers (RFC 8707)

Status: planned — not started. Written 2026-11-20 after a live probe
against an OAuth-armed MCP appliance (librarium, which enforces resource
indicators per the MCP authorization spec). This document is the outline
for a future fix process; no code has changed.

## The problem

`mcp --auth client-credentials` cannot talk to MCP authorization servers
that follow the spec's resource-indicator model (RFC 8707 / RFC 9728).
Two independent layers fail, both observed live:

### Layer 1 — SDK protected-resource-metadata validation rejects the connect

```
ERROR: connecting to http://127.0.0.1:18105/mcp failed in 9ms:
  calling "initialize": sending "initialize": rejected by transport:
  fetching protected resource metadata:
  GetProtectedResourceMetadata("http://127.0.0.1:18105/.well-known/oauth-protected-resource"):
  got metadata resource "http://127.0.0.1:18105",
  want "http://127.0.0.1:18105/mcp"
```

The server's PRM document advertises `resource = <issuer>` (the root),
while the SDK client insists the advertised resource equal the URL it is
connecting to (`<issuer>/mcp`). Per RFC 9728 the PRM's `resource` field
identifies the protected resource — the client should REQUEST a token for
the advertised resource, not demand it match the endpoint URL. The token
endpoint on the probed server accepts BOTH forms ({issuer, issuer+"/mcp"}),
so the rigidity is client-side.

### Layer 2 — the handler never sends the resource parameter

`internal/auth/clientcred.go` builds

```go
clientcredentials.Config{ClientID: …, ClientSecret: …, TokenURL: …, Scopes: …}
```

with no `EndpointParams` — the `resource` form value is never sent
(repo-wide: zero hits). Servers that REQUIRE the indicator answer:

```
{"error":"invalid_target","error_description":"resource is required and
 must be \"http://127.0.0.1:18105\" or \"http://127.0.0.1:18105/mcp\""}
```

This is the same omission the appliance's project already recorded for
the official go-sdk's own handler (its corpus harness ships a custom
`auth.OAuthHandler` for exactly this reason). Probe reference:
librarium v0.2.1-class appliance, mcp built from c9476ae.

## Workaround that works today

`--auth bearer` with a manually fetched token (reuses
`--client-secret` / `MCPCLI_CLIENT_SECRET` as the static token):

```sh
TOKEN=$(curl -s "$BASE/oauth/token" -u "$ID:$SECRET" \
  -d grant_type=client_credentials -d "resource=$BASE" | …)
MCPCLI_CLIENT_SECRET="$TOKEN" mcp --url "$BASE/mcp" --auth bearer …
```

## Fix outline

1. **Respect the PRM-advertised resource** (layer 1): during the
   protected-resource metadata fetch, accept the advertised `resource`
   as the token's target instead of requiring it to equal the connection
   URL. If the validation lives in the go-sdk client (likely — the same
   place librarium's harness worked around), this may need an upstream
   issue plus a local workaround in the interim, mirroring the harness's
   custom-handler pattern.
2. **Send the resource parameter** (layer 2): populate
   `EndpointParams: url.Values{"resource": {…}}` in the
   client-credentials `Config`, value = the PRM-advertised resource when
   present, else the server URL; a `--resource` flag overrides for
   explicitness. Env-var secret form unaffected.
3. **Tests**: unit — the built `Config` carries the expected
   `EndpointParams` (both value sources + flag override); e2e — against
   a resource-requiring authorization server (librarium is the reference
   appliance; its harness handler is prior art for the workaround shape).

## Acceptance

`mcp --url <base>/mcp --auth client-credentials --client-id ID
--client-secret SECRET tools list` succeeds against an OAuth-armed
librarium whose PRM advertises the issuer form; no bearer pre-step; the
plain-http and command-line-secret warnings still fire as today.
