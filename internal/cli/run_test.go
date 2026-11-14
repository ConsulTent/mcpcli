package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/bws"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestMCPServer hosts an in-memory MCP server behind the streamable HTTP
// handler — the same topology internal/connection's tests use — so the cli
// E2E tests exercise a real initialize handshake over real HTTP.
func newTestMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "testserver", Version: "1.0.0"}, nil)
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

// runRoot executes the root command with args, capturing what the command
// writes to stdout and stderr (via cobra's streams) and what the ui layer
// decorates onto its own output (normally stderr). Tests must stay sequential:
// both the ui mode and the ui output writer are package globals.
func runRoot(t *testing.T, args ...string) (stdout, stderr, decoration string, err error) {
	t.Helper()
	var out, errBuf, dec bytes.Buffer
	restore := ui.SetSpinnerOutput(&dec)
	defer restore()

	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errBuf.String(), dec.String(), err
}

// hostileESC is the planted terminal-escape payload every human-mode render
// sink must strip: an OSC title-setter plus a color-introducer, the two
// shapes a server-controlled string could use to hijack the operator's
// terminal.
const hostileESC = "\x1b]0;pwned\x07\x1b[31m"

// assertNoESC fails when s carries the ANSI escape byte (0x1b) — the hostile
// byte the printable-character filters must strip from server-supplied text.
func assertNoESC(t *testing.T, name, s string) {
	t.Helper()
	if i := strings.IndexByte(s, 0x1b); i >= 0 {
		t.Fatalf("%s has an ESC byte at offset %d: %q", name, i, s)
	}
}

// TestRunWithConnectionTarget pins the target enforcement the shared runner
// applies before anything else, so connection commands fail fast without
// touching the network while auth logout stays offline-capable.
func TestRunWithConnectionTarget(t *testing.T) {
	_, _, _, err := runRoot(t, "info")
	if err == nil || !strings.Contains(err.Error(), "--command") || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("info without a target = %v, want an error naming --command and --url", err)
	}

	_, _, _, err = runRoot(t, "--command", "some-server", "--url", "http://localhost:1/mcp", "info")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("info with both targets = %v, want a mutual-exclusion error", err)
	}
}

func TestInfoE2E(t *testing.T) {
	srv := newTestMCPServer(t)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "info")
	if err != nil {
		t.Fatalf("info: %v", err)
	}

	// The server's self-reported name and version from the initialize result.
	if !strings.Contains(stdout, "testserver") {
		t.Errorf("info stdout %q does not name the server testserver", stdout)
	}
	// The human rendering ends with the client's copyright footer.
	if !strings.Contains(stdout, "Copyright ConsulTent Pte. Ltd., all Rights Reserved.") ||
		!strings.Contains(stdout, "https://consultent.ltd") {
		t.Errorf("info stdout %q does not carry the copyright footer", stdout)
	}
	if !strings.Contains(stdout, "1.0.0") {
		t.Errorf("info stdout %q does not report the server version", stdout)
	}
	// The negotiated protocol version: reported non-empty, and a version the
	// linked SDK speaks. (Which version auto-negotiation lands on is an SDK
	// transport detail — over stateful streamable HTTP it is not simply the
	// SDK's latest — so the test pins the contract, not the exact value.)
	var gotProto string
	for _, line := range strings.Split(stdout, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "Protocol:") {
			gotProto = strings.TrimSpace(strings.TrimPrefix(t, "Protocol:"))
		}
	}
	if gotProto == "" {
		t.Fatalf("info stdout %q does not report a protocol version", stdout)
	}
	if !slices.Contains(mcp.SupportedProtocolVersions(), gotProto) {
		t.Errorf("negotiated protocol version %q is not one the SDK supports: %v", gotProto, mcp.SupportedProtocolVersions())
	}
}

func TestPingE2E(t *testing.T) {
	srv := newTestMCPServer(t)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !strings.Contains(stdout, "pong") {
		t.Errorf("ping stdout %q does not report pong", stdout)
	}
}

// TestPingPinned2026E2E: a ping on a 2026-07-28 session must succeed — opPing
// sets the SEP-2575 _meta.protocolVersion explicitly, because the SDK's Ping
// never injects it and servers validating header/meta consistency reject a
// meta-less request with -32020.
func TestPingPinned2026E2E(t *testing.T) {
	srv := newTestMCPServer(t)

	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "--protocol", "2026-07-28", "ping")
	if err != nil {
		t.Fatalf("pinned 2026-07-28 ping: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "pong") {
		t.Errorf("pinned 2026-07-28 ping stdout %q does not report pong", stdout)
	}
}

func TestJSONNoDecoration(t *testing.T) {
	srv := newTestMCPServer(t)

	// ping --json: exactly the machine-readable result, nothing on stderr.
	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--json", "ping")
	if err != nil {
		t.Fatalf("ping --json: %v", err)
	}
	var pong struct {
		OK        bool  `json:"ok"`
		LatencyMS int64 `json:"latency_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &pong); err != nil {
		t.Fatalf("ping --json stdout %q is not JSON: %v", stdout, err)
	}
	if !pong.OK {
		t.Errorf("ping --json ok = false, want true")
	}
	if stderr != "" || dec != "" {
		t.Errorf("ping --json wrote decoration: stderr %q, ui output %q", stderr, dec)
	}

	// info --json: the raw InitializeResult, also without decoration.
	stdout, stderr, dec, err = runRoot(t, "--url", srv.URL, "--json", "info")
	if err != nil {
		t.Fatalf("info --json: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("info --json stdout %q is not JSON: %v", stdout, err)
	}
	if _, ok := res["protocolVersion"]; !ok {
		t.Errorf("info --json output %q lacks protocolVersion", stdout)
	}
	if stderr != "" || dec != "" {
		t.Errorf("info --json wrote decoration: stderr %q, ui output %q", stderr, dec)
	}
}

// TestPlainNoSpecialChars runs both session commands end-to-end in Plain mode
// and asserts every captured byte is ASCII with no escape sequences.
func TestPlainNoSpecialChars(t *testing.T) {
	srv := newTestMCPServer(t)

	for _, args := range [][]string{
		{"--url", srv.URL, "--plain", "info"},
		{"--url", srv.URL, "--plain", "ping"},
	} {
		stdout, stderr, dec, err := runRoot(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for name, s := range map[string]string{"stdout": stdout, "stderr": stderr, "ui output": dec} {
			for i := 0; i < len(s); i++ {
				if b := s[i]; b >= 0x80 {
					t.Fatalf("%v: %s has non-ASCII byte 0x%02x at offset %d: %q", args, name, b, i, s)
				}
				if b := s[i]; b == 0x1b {
					t.Fatalf("%v: %s has an ESC byte at offset %d: %q", args, name, i, s)
				}
			}
		}
	}
}

// TestBearerRefFlowE2E pins the bearer path through a secret reference:
// --client-secret bws://<uuid> resolves through the fake BWS BEFORE the
// connection opens, the MCP server observes the RESOLVED value in the
// Authorization header, the identity exchange is paid once per run (the
// Source's client memo), and the always-on instance note prints exactly
// once — while a run without references prints none.
func TestBearerRefFlowE2E(t *testing.T) {
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
	// The env selector path (dummies via t.Setenv): the expansion resolves
	// MCPCLI_BWS_API_URL, not just the cache.
	t.Setenv("MCPCLI_BWS_API_URL", fa.url)

	srv := mcp.NewServer(&mcp.Implementation{Name: "testserver", Version: "1.0.0"}, nil)
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	var gotAuth string
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		streamable.ServeHTTP(w, r)
	}))
	t.Cleanup(httpSrv.Close)

	stdout, stderr, _, err := runRoot(t, "--url", httpSrv.URL, "--auth", "bearer",
		"--client-secret", "bws://"+uuidRef, "ping")
	if err != nil {
		t.Fatalf("ping with a bws:// bearer ref: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "pong") {
		t.Fatalf("ping stdout %q does not report pong", stdout)
	}
	if gotAuth != "Bearer value-a" {
		t.Fatalf("Authorization header = %q, want the resolved value", gotAuth)
	}
	if n := fi.exchangeHits(); n != 1 {
		t.Errorf("identity exchange hit %d times, want 1 (memoized per run)", n)
	}
	if got := strings.Count(stderr, "bws: using instance "+fa.url); got != 1 {
		t.Errorf("instance note count = %d, want exactly 1\nstderr:\n%s", got, stderr)
	}

	// A run without any reference stays off the expansion path: no note.
	_, stderr, _, err = runRoot(t, "--url", httpSrv.URL, "ping")
	if err != nil {
		t.Fatalf("plain ping: %v\nstderr: %s", err, stderr)
	}
	if strings.Contains(stderr, "bws: using instance") {
		t.Errorf("plain ping printed the instance note\nstderr:\n%s", stderr)
	}
}
