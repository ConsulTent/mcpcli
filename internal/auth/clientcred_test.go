package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientCredMissing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, err := ProviderFor("client-credentials")
	if err != nil {
		t.Fatalf("ProviderFor(client-credentials): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{
		Target: srv.URL,
		Creds:  Credentials{},
	})
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected error for missing client credentials, got nil")
	}
	if h != nil {
		t.Error("handler must be nil on error")
	}
	for _, want := range []string{"--client-id", "--client-secret", "MCPCLI_CLIENT_ID", "MCPCLI_CLIENT_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("Build made %d network requests, want 0", n)
	}
}

// TestClientCredAuthorizeFlow runs the full discovery + token flow against a
// fake IdP served by a single httptest server:
//
//	/mcp                                    -> 401 + WWW-Authenticate resource_metadata
//	/.well-known/oauth-protected-resource   -> PRM doc naming the authorization server
//	/.well-known/oauth-authorization-server -> AS metadata doc with the token endpoint
//	/token                                  -> token endpoint issuing a dummy access token
func TestClientCredAuthorizeFlow(t *testing.T) {
	const (
		access = "test-access-token"
		id     = "test-client-id"
		secret = "dummy-secret"
	)

	var srvURL string
	var tokenHits atomic.Int32

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="`+srvURL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              srvURL + "/mcp",
			"authorization_servers": []string{srvURL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           srvURL,
			"authorization_endpoint":           srvURL + "/authorize",
			"token_endpoint":                   srvURL + "/token",
			"jwks_uri":                         srvURL + "/jwks",
			"scopes_supported":                 []string{"mcp:read", "mcp:write"},
			"grant_types_supported":            []string{"client_credentials"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("token endpoint got %s, want POST", r.Method)
		}
		if r.FormValue("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", r.FormValue("grant_type"))
		}
		// oauth2 auto-detect starts with Basic auth; accept either style.
		gotID, gotSecret, ok := r.BasicAuth()
		if !ok {
			gotID, gotSecret = r.FormValue("client_id"), r.FormValue("client_secret")
		}
		if gotID != id || gotSecret != secret {
			t.Errorf("client auth = %q/%q, want %q/%q", gotID, gotSecret, id, secret)
		}
		if gotScope := r.FormValue("scope"); gotScope != "mcp:read mcp:write" {
			t.Errorf("scope = %q, want %q (scopes advertised in AS metadata)", gotScope, "mcp:read mcp:write")
		}
		tokenHits.Add(1)
		writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	t.Setenv("MCPCLI_CLIENT_ID", id)
	t.Setenv("MCPCLI_CLIENT_SECRET", secret)

	p, err := ProviderFor("client-credentials")
	if err != nil {
		t.Fatalf("ProviderFor(client-credentials): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{
		Target: srvURL + "/mcp",
		Creds:  ResolveCredentials("", ""),
	})
	if err != nil {
		t.Fatalf("client-credentials Build: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Undiscovered: no token source, so the transport sends no header and
	// the server 401 drives discovery.
	ts0, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource before Authorize: %v", err)
	}
	if ts0 != nil {
		t.Fatal("TokenSource must be nil before Authorize discovers the endpoint")
	}

	// Trigger the 401 like the SDK transport would.
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srvURL+"/mcp", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to fake MCP server: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// Authorize performs discovery + client-credentials grant. Per the SDK
	// contract it owns (and closes) the response body.
	if err := h.Authorize(ctx, req, resp); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	resp.Body.Close()

	// After a successful Authorize, TokenSource must return a live source.
	ts, err := h.TokenSource(ctx)
	if err != nil {
		t.Fatalf("TokenSource after Authorize: %v", err)
	}
	if ts == nil {
		t.Fatal("TokenSource after successful Authorize is nil; SDK would retry headerless")
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("ts.Token(): %v", err)
	}
	if tok.AccessToken != access {
		t.Errorf("access token = %q, want %q", tok.AccessToken, access)
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times, want 1", n)
	}
}

func TestClientCredAuthorizeDiscoveryFailure(t *testing.T) {
	// A 401 without a WWW-Authenticate challenge and no metadata anywhere:
	// Authorize must fail with a clear error rather than hang or panic.
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	t.Setenv("MCPCLI_CLIENT_ID", "test-client-id")
	t.Setenv("MCPCLI_CLIENT_SECRET", "dummy-secret")

	p, err := ProviderFor("client-credentials")
	if err != nil {
		t.Fatalf("ProviderFor(client-credentials): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{
		Target: srvURL + "/mcp",
		Creds:  ResolveCredentials("", ""),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srvURL+"/mcp", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if err := h.Authorize(ctx, req, resp); err == nil {
		t.Fatal("expected Authorize error when no metadata is discoverable")
	}
}
