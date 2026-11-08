package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/bws"
	"github.com/kballard/go-shellquote"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The expansion-path tests: the expandSecretRefs helper's unit behavior and
// its three consumers (runWithConnection via the bearer flow in run_test.go,
// runJevCheck's own session open, and auth login). Every credential-shaped
// value is an obviously-synthetic dummy riding the house fakes from
// bws_test.go.

// bwsTestSeed resolves the standard one-secret fixture into the cache: uuid
// ref "11111111-..." → decrypted name "first", value "value-a".
func bwsTestSeed(t *testing.T) {
	t.Helper()
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	uuidRef := "11111111-2222-3333-4444-555555555555"
	fa := newBWSTestAPI(t, orgKey, map[string]bws.Secret{
		uuidRef: {
			ID: uuidRef, OrganizationID: bwsTestOrgID, Key: "first", Value: "value-a", CanRead: true,
		},
	})
	fi := newBWSTestIdentity(t)
	if err := bws.SaveCredential(bws.Credential{
		APIToken:    bwsTestToken,
		OrgID:       bwsTestOrgID,
		APIURL:      fa.url,
		IdentityURL: fi.url,
	}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
}

// TestExpandSecretRefsLiteralEnv: a stdio run's --env map carries every
// binding — literals pass through unchanged, empty values stay empty — and
// a literal ClientSecret passes through untouched.
func TestExpandSecretRefsLiteralEnv(t *testing.T) {
	f := &GlobalFlags{Command: "some-server"}
	f.envBindings = []envBinding{
		{Key: "PLAIN", Value: "plain-value"},
		{Key: "EMPTY", Value: ""},
	}
	f.ClientSecret = "literal-bearer-token"

	env, err := expandSecretRefs(context.Background(), f, io.Discard)
	if err != nil {
		t.Fatalf("expandSecretRefs: %v", err)
	}
	if env["PLAIN"] != "plain-value" {
		t.Errorf("env[PLAIN] = %q, want the literal", env["PLAIN"])
	}
	v, ok := env["EMPTY"]
	if !ok || v != "" {
		t.Errorf("env[EMPTY] = %q, %v, want an empty entry", v, ok)
	}
	if f.ClientSecret != "literal-bearer-token" {
		t.Errorf("ClientSecret mutated to %q, want the literal unchanged", f.ClientSecret)
	}
}

// TestExpandSecretRefsHTTPIgnoresEnv: an HTTP run (Command == "") must not
// expand --env values — the map would be dead, and each ref would cost a
// live identity POST. The temp TokenDir holds no credential, so any BWS
// consult fails the test loudly instead of passing silently.
func TestExpandSecretRefsHTTPIgnoresEnv(t *testing.T) {
	bwsTestTokenDir(t)
	f := &GlobalFlags{URL: "https://example.invalid/mcp"}
	f.envBindings = []envBinding{{Key: "K", Value: "bws://name"}}

	env, err := expandSecretRefs(context.Background(), f, io.Discard)
	if err != nil {
		t.Fatalf("expandSecretRefs on an HTTP run: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("env = %v, want empty (the map is dead on HTTP runs)", env)
	}
}

// TestExpandSecretRefsRejectsNUL pins the shared NUL guard in unit form:
// both the --env loop and the ClientSecret expansion reject a NUL-bearing
// value naming the attribute — a NUL would otherwise die later as an
// unattributable exec or net/http error.
func TestExpandSecretRefsRejectsNUL(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		f := &GlobalFlags{Command: "some-server"}
		f.envBindings = []envBinding{{Key: "K", Value: "bad\x00value"}}
		_, err := expandSecretRefs(context.Background(), f, io.Discard)
		if err == nil {
			t.Fatal("NUL-bearing env value accepted")
		}
		for _, sub := range []string{"--env K", "NUL"} {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q does not contain %q", err, sub)
			}
		}
	})
	t.Run("client-secret", func(t *testing.T) {
		f := &GlobalFlags{URL: "https://example.invalid/mcp", ClientSecret: "bad\x00secret"}
		_, err := expandSecretRefs(context.Background(), f, io.Discard)
		if err == nil {
			t.Fatal("NUL-bearing client secret accepted")
		}
		for _, sub := range []string{"--client-secret", "NUL"} {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q does not contain %q", err, sub)
			}
		}
	})
}

// TestEnvNULRefGolden: a fake secret whose decrypted value carries NUL fails
// a stdio run naming the KEY (the bws guard surfacing through the cli wrap).
func TestEnvNULRefGolden(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	nulRef := "22222222-2222-3333-4444-555555555555"
	fa := newBWSTestAPI(t, orgKey, map[string]bws.Secret{
		nulRef: {
			ID: nulRef, OrganizationID: bwsTestOrgID, Key: "nul-key", Value: "has\x00nul", CanRead: true,
		},
	})
	fi := newBWSTestIdentity(t)
	if err := bws.SaveCredential(bws.Credential{
		APIToken:    bwsTestToken,
		OrgID:       bwsTestOrgID,
		APIURL:      fa.url,
		IdentityURL: fi.url,
	}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	_, _, _, err := runRoot(t, "--env", "K=bws://"+nulRef, "--command", "some-server", "ping")
	if err == nil {
		t.Fatal("NUL-bearing secret ref accepted")
	}
	for _, sub := range []string{"--env K", "nul-key", "NUL"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err, sub)
		}
	}
}

// TestClientSecretNULRefGolden: the same NUL secret behind --client-secret
// fails before any connection, attributed to the flag.
func TestClientSecretNULRefGolden(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	nulRef := "22222222-2222-3333-4444-555555555555"
	fa := newBWSTestAPI(t, orgKey, map[string]bws.Secret{
		nulRef: {
			ID: nulRef, OrganizationID: bwsTestOrgID, Key: "nul-key", Value: "has\x00nul", CanRead: true,
		},
	})
	fi := newBWSTestIdentity(t)
	if err := bws.SaveCredential(bws.Credential{
		APIToken:    bwsTestToken,
		OrgID:       bwsTestOrgID,
		APIURL:      fa.url,
		IdentityURL: fi.url,
	}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	_, _, _, err := runRoot(t, "--url", "https://example.invalid/mcp", "--auth", "bearer",
		"--client-secret", "bws://"+nulRef, "ping")
	if err == nil {
		t.Fatal("NUL-bearing secret ref accepted as the client secret")
	}
	for _, sub := range []string{"--client-secret", "nul-key", "NUL"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err, sub)
		}
	}
}

// cliJevHelperKey marks this test binary as wanting to run
// TestHelperProcessJevEnv as a stdio jkudish server that only boots when the
// injected env var overrode the inherited one.
const cliJevHelperKey = "GO_MCPCLI_JEV_ENV_HELPER"

// TestHelperProcessJevEnv serves the jkudish fixture in helper mode — but
// only after asserting the child sees the RESOLVED secret value: a wiring
// hole in runJevCheck's own Options construction would leave the inherited
// value in place, and the child dies naming what it saw instead (the
// diagnostic rides the connect error's subprocess stderr).
func TestHelperProcessJevEnv(t *testing.T) {
	if os.Getenv(cliJevHelperKey) != "1" {
		return
	}
	if got := os.Getenv("JEV_HELPER_SECRET"); got != "value-a" {
		_, _ = fmt.Fprintf(os.Stderr, "helper: JEV_HELPER_SECRET=%q, want the injected \"value-a\"\n", got)
		os.Exit(3)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "jev-env-helper", Version: "0.9.0"}, nil)
	addJkudishFixture(srv, nil)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestJevCheckInjectsEnv pins the jev session-open wiring: the --env ref
// resolves through the fake BWS, the injected map rides runJevCheck's own
// connection.Options (not just runWithConnection's), and the instance note
// prints once on the expansion path.
func TestJevCheckInjectsEnv(t *testing.T) {
	bwsTestSeed(t)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	t.Setenv(cliJevHelperKey, "1")
	t.Setenv("JEV_HELPER_SECRET", "inherited")

	uuidRef := "11111111-2222-3333-4444-555555555555"
	stdout, stderr, _, err := runRoot(t,
		"--command", shellquote.Join(exe, "-test.run=TestHelperProcessJevEnv"),
		"--env", "JEV_HELPER_SECRET=bws://"+uuidRef,
		"--json", "jev", "check")
	if err != nil {
		t.Fatalf("jev check with injected env: %v\nstderr: %s", err, stderr)
	}
	doc := decodeJevCheck(t, stdout)
	if doc.Flavor != "jkudish" {
		t.Fatalf("flavor = %q, want jkudish (the child booted only with the injected value)", doc.Flavor)
	}
	if got := strings.Count(stderr, "bws: using instance"); got != 1 {
		t.Errorf("instance note count = %d, want exactly 1\nstderr:\n%s", got, stderr)
	}
}

// TestAuthLoginExpandsClientSecretRef is the third-consumer pin: auth login
// expands the --client-secret ref before its OAuth params are built, so the
// token exchange carries the RESOLVED secret; the tightened --auth-flow
// exemption still admits auth login, and the ref stays argv-warning silent.
func TestAuthLoginExpandsClientSecretRef(t *testing.T) {
	bwsTestSeed(t)

	const code = "test-auth-code"
	target, tokenHits, _ := newFakeIdPServer(t, "test-access-token", code, "http://127.0.0.1:1/callback", "value-a")

	t.Setenv("MCPCLI_CLIENT_ID", "test-client-id")
	uuidRef := "11111111-2222-3333-4444-555555555555"
	stderr, err := runAuthCommand(strings.NewReader(code+"\n"),
		"auth", "login", "--url", target+"/mcp", "--auth-flow", "manual",
		"--client-secret", "bws://"+uuidRef, "--timeout", "30s")
	if err != nil {
		t.Fatalf("auth login with a bws:// client secret: %v\nstderr: %s", err, stderr)
	}
	if n := tokenHits.Load(); n != 1 {
		t.Errorf("token endpoint hit %d times, want 1", n)
	}
	if strings.Contains(stderr, clientSecretWarning) {
		t.Errorf("ref-sourced --client-secret fired the argv warning\nstderr:\n%s", stderr)
	}
}

// TestExpandSecretRefsUncachedGolden: an expansion with no cached credential
// fails before any connection, wrapping the empty-flag resolution error —
// which carries the "run mcp bws login" remediation — with the failing KEY.
func TestExpandSecretRefsUncachedGolden(t *testing.T) {
	bwsTestTokenDir(t) // empty cache, no MCPCLI_BWS_API_URL
	_, _, _, err := runRoot(t, "--env", "K=bws://name", "--command", "some-server", "ping")
	if err == nil {
		t.Fatal("uncached expansion accepted")
	}
	for _, sub := range []string{"--env K", "mcp bws login"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err, sub)
		}
	}
}
