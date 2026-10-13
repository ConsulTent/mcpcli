package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// runAuthCommand executes the root command with the given args and scripted
// stdin, returning everything the command wrote to stderr. A nil stdin
// becomes an empty reader, so a command that unexpectedly reads stdin gets
// EOF rather than the test process's input.
func runAuthCommand(stdin io.Reader, args ...string) (string, error) {
	var out, errBuf bytes.Buffer
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	root.SetIn(stdin)
	root.SetArgs(args)
	err := root.Execute()
	return errBuf.String(), err
}

// newFakeIdPServer hosts one httptest server that plays every role in the
// OAuth topology, the same single-server layout as the Task 5 flow test:
//
//	/mcp                                    -> gated real MCP streamable endpoint;
//	                                           401 + WWW-Authenticate without a valid token
//	/.well-known/oauth-protected-resource   -> PRM doc naming the authorization server
//	/.well-known/oauth-authorization-server -> AS metadata with the token endpoint
//	/token                                  -> authorization-code token endpoint
func newFakeIdPServer(t *testing.T, access, wantCode, wantRedirect string) (srvURL string, tokenHits, authedHits *atomic.Int32) {
	t.Helper()

	var urlHolder atomic.Value // string, set once the server URL is known
	url := func() string { return urlHolder.Load().(string) }

	tokenHits = &atomic.Int32{}
	authedHits = &atomic.Int32{}

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}

	// A real in-memory MCP server behind the auth gate, so login performs a
	// genuine initialize + ping handshake after authorizing.
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "oauth-test-server", Version: "1.0.0"}, nil)
	streamable := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+access {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+url()+`/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authedHits.Add(1)
		streamable.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              url() + "/mcp",
			"authorization_servers": []string{url()},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           url(),
			"authorization_endpoint":           url() + "/authorize",
			"token_endpoint":                   url() + "/token",
			"jwks_uri":                         url() + "/jwks",
			"scopes_supported":                 []string{"mcp:read"},
			"grant_types_supported":            []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q, want authorization_code", r.FormValue("grant_type"))
		}
		if got := r.FormValue("code"); got != wantCode {
			t.Errorf("code = %q, want %q", got, wantCode)
		}
		if got := r.FormValue("redirect_uri"); got != wantRedirect {
			t.Errorf("redirect_uri = %q, want %q", got, wantRedirect)
		}
		// oauth2 auto-detect starts with Basic auth; accept either style.
		gotID, _, ok := r.BasicAuth()
		if !ok {
			gotID = r.FormValue("client_id")
		}
		if gotID != "test-client-id" {
			t.Errorf("client_id = %q, want %q", gotID, "test-client-id")
		}
		tokenHits.Add(1)
		writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	urlHolder.Store(srv.URL)
	return srv.URL, tokenHits, authedHits
}

func TestAuthLoginSavesToken(t *testing.T) {
	oldDir := auth.TokenDir
	auth.TokenDir = t.TempDir()
	t.Cleanup(func() { auth.TokenDir = oldDir })

	const (
		access = "test-access-token"
		code   = "test-auth-code"
	)

	target, tokenHits, authedHits := newFakeIdPServer(t, access, code, "http://127.0.0.1:1/callback")

	t.Setenv("MCPCLI_CLIENT_ID", "test-client-id")
	t.Setenv("MCPCLI_CLIENT_SECRET", "dummy-secret")

	// Manual flow with a scripted paste: no browser, no network beyond the
	// loopback test servers.
	stderr, err := runAuthCommand(strings.NewReader(code+"\n"),
		"auth", "login", "--url", target+"/mcp", "--auth-flow", "manual", "--timeout", "30s")
	if err != nil {
		t.Fatalf("auth login: %v", err)
	}
	if !strings.Contains(stderr, "authorize at") {
		t.Errorf("login stderr %q does not announce the authorization URL", stderr)
	}
	if !strings.Contains(stderr, "logged in") {
		t.Errorf("login stderr %q does not report success", stderr)
	}

	tok, err := auth.LoadToken(target + "/mcp")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if tok == nil || tok.AccessToken != access {
		t.Fatalf("LoadToken after login = %+v, want the issued access token %q", tok, access)
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times, want 1", n)
	}
	if n := authedHits.Load(); n < 1 {
		t.Errorf("MCP endpoint never received an authorized request")
	}

	// A second login in JSON mode reuses the cached token: silent stderr, no
	// second token exchange, no re-authorization.
	authedBefore := authedHits.Load()
	stderr, err = runAuthCommand(nil, "auth", "login", "--url", target+"/mcp", "--json", "--timeout", "30s")
	if err != nil {
		t.Fatalf("second auth login (JSON): %v", err)
	}
	if stderr != "" {
		t.Errorf("JSON-mode login wrote %q to stderr, want silence", stderr)
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times after cached-token login, want 1", n)
	}
	if authedHits.Load() <= authedBefore {
		t.Error("cached-token login never reached the MCP endpoint")
	}
}

func TestAuthLoginRejectsStdio(t *testing.T) {
	_, err := runAuthCommand(nil, "auth", "login", "--command", "some-server")
	if err == nil {
		t.Fatal("auth login with --command returned nil error")
	}
	if !strings.Contains(err.Error(), "--url") {
		t.Errorf("error %v does not point at --url", err)
	}
}

func TestAuthLoginRejectsOtherAuth(t *testing.T) {
	_, err := runAuthCommand(nil, "auth", "login", "--url", "https://example.test/mcp", "--auth", "bearer")
	if err == nil {
		t.Fatal("auth login with --auth bearer returned nil error")
	}
	if !strings.Contains(err.Error(), "oauth") {
		t.Errorf("error %v does not name the oauth flow", err)
	}
}

// fakeAccessTokenValue derives an obviously-synthetic, per-test token value
// so no credential-shaped literal appears in the test source.
func fakeAccessTokenValue(t *testing.T) string {
	return "not-a-real-token-" + t.Name()
}

func TestAuthLogoutDeletesToken(t *testing.T) {
	oldDir := auth.TokenDir
	auth.TokenDir = t.TempDir()
	t.Cleanup(func() { auth.TokenDir = oldDir })

	const target = "https://example.test/mcp"
	if err := auth.SaveToken(target, &oauth2.Token{AccessToken: fakeAccessTokenValue(t), TokenType: "Bearer"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	stderr, err := runAuthCommand(nil, "auth", "logout", "--url", target)
	if err != nil {
		t.Fatalf("auth logout: %v", err)
	}
	if !strings.Contains(stderr, "logged out") {
		t.Errorf("logout stderr %q does not report success", stderr)
	}

	tok, err := auth.LoadToken(target)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if tok != nil {
		t.Errorf("LoadToken after logout = %+v, want nil", tok)
	}
}

func TestAuthLogoutByCommand(t *testing.T) {
	oldDir := auth.TokenDir
	auth.TokenDir = t.TempDir()
	t.Cleanup(func() { auth.TokenDir = oldDir })

	const target = `npx -y @modelcontextprotocol/server-filesystem /tmp`
	if err := auth.SaveToken(target, &oauth2.Token{AccessToken: fakeAccessTokenValue(t), TokenType: "Bearer"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	// Runs with no server anywhere in the test: logout is strictly offline.
	if _, err := runAuthCommand(nil, "auth", "logout", "--command", target); err != nil {
		t.Fatalf("auth logout --command: %v", err)
	}
	if tok, _ := auth.LoadToken(target); tok != nil {
		t.Errorf("LoadToken after logout = %+v, want nil", tok)
	}
}

func TestAuthLogoutTargetRequired(t *testing.T) {
	_, err := runAuthCommand(nil, "auth", "logout")
	if err == nil {
		t.Fatal("auth logout without --url or --command returned nil error")
	}
	if !strings.Contains(err.Error(), "--url") || !strings.Contains(err.Error(), "--command") {
		t.Errorf("error %v does not name both target flags", err)
	}
}
