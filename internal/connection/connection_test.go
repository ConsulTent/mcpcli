package connection

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kballard/go-shellquote"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// stubOAuthHandler satisfies sdkauth.OAuthHandler so tests can pass a
// non-nil handler without any real OAuth machinery.
type stubOAuthHandler struct{}

var _ sdkauth.OAuthHandler = stubOAuthHandler{}

func (stubOAuthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) { return nil, nil }

func (stubOAuthHandler) Authorize(context.Context, *http.Request, *http.Response) error {
	return nil
}

// TestOpenHintsAuthOn401: an unauthorized connect with --auth none carries the
// retry hint, so the first-run dead end (server demands auth, user sees a bare
// 401) tells them what to do next.
func TestOpenHintsAuthOn401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	_, err := Open(context.Background(), &Options{URL: srv.URL, Auth: "none", ProtocolVersion: "auto"})
	if err == nil {
		t.Fatal("Open against a 401 server = nil error, want failure")
	}
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "unauthorized") {
		t.Errorf("error %q does not mention the unauthorized response", msg)
	}
	if !strings.Contains(msg, "retry with --auth bearer or --auth oauth") {
		t.Errorf("error %q does not carry the auth retry hint", msg)
	}
}

// TestOpenNoHintWhenAuthConfigured: the hint is only for the --auth none
// case — a run that already asked for auth must not be told to add more.
func TestOpenNoHintWhenAuthConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	_, err := Open(context.Background(), &Options{URL: srv.URL, Auth: "bearer", ProtocolVersion: "auto"})
	if err == nil {
		t.Fatal("Open against a 401 server = nil error, want failure")
	}
	if strings.Contains(err.Error(), "retry with --auth") {
		t.Errorf("error %q carries the hint despite --auth bearer", err.Error())
	}
}

func TestValidateMutuallyExclusive(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"command only", Options{Command: "some-server"}, false},
		{"url only", Options{URL: "http://localhost:8080/mcp"}, false},
		{"both", Options{Command: "some-server", URL: "http://localhost:8080/mcp"}, true},
		{"neither", Options{}, true},
	}
	for _, tc := range cases {
		err := tc.opts.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("%s: Validate() = nil, want error", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: Validate() = %v, want nil", tc.name, err)
		}
	}
}

func TestValidateStdioAuth(t *testing.T) {
	err := (&Options{Command: "some-server", AuthHandler: stubOAuthHandler{}}).Validate()
	if err == nil {
		t.Fatal("Validate() with --command + AuthHandler = nil, want error")
	}

	// OAuth over streamable HTTP is fine.
	if err := (&Options{URL: "http://localhost:8080/mcp", AuthHandler: stubOAuthHandler{}}).Validate(); err != nil {
		t.Fatalf("Validate() with --url + AuthHandler = %v, want nil", err)
	}
	// stdio without auth handler is fine.
	if err := (&Options{Command: "some-server"}).Validate(); err != nil {
		t.Fatalf("Validate() with --command only = %v, want nil", err)
	}
}

// newTestHTTPServer hosts an in-memory MCP server behind the streamable HTTP
// handler, the same way a real remote server would.
func newTestHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "testserver", Version: "1.0.0"}, nil)
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

func TestOpenStreamableHTTP(t *testing.T) {
	httpSrv := newTestHTTPServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := Open(ctx, &Options{URL: httpSrv.URL, ProtocolVersion: "auto"})
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer session.Close()

	if err := session.Ping(ctx, nil); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}

	res := session.InitializeResult()
	if res.ServerInfo.Name != "testserver" || res.ServerInfo.Version != "1.0.0" {
		t.Fatalf("InitializeResult().ServerInfo = %s/%s, want testserver/1.0.0", res.ServerInfo.Name, res.ServerInfo.Version)
	}
	if res.ProtocolVersion == "" {
		t.Fatal("InitializeResult().ProtocolVersion is empty, want a negotiated version")
	}
}

func TestOpenStreamableHTTPPinnedVersion(t *testing.T) {
	httpSrv := newTestHTTPServer(t)

	// Pin a version from the middle of the SDK's supported list so the test
	// proves the pin was sent, rather than defaulting to the newest.
	const pinned = "2025-06-18"
	versions := mcp.SupportedProtocolVersions()
	if !func() bool {
		for _, v := range versions {
			if v == pinned {
				return true
			}
		}
		return false
	}() {
		t.Skipf("SDK %v does not support %q; pin a version it accepts", versions, pinned)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := Open(ctx, &Options{URL: httpSrv.URL, ProtocolVersion: pinned})
	if err != nil {
		t.Fatalf("Open() with pinned %s error: %v", pinned, err)
	}
	defer session.Close()

	res := session.InitializeResult()
	if res.ProtocolVersion != pinned {
		t.Fatalf("InitializeResult().ProtocolVersion = %q, want pinned %q", res.ProtocolVersion, pinned)
	}
}

// helperEnvKey marks this test binary as wanting to run TestHelperProcess as
// an MCP stdio server subprocess. Value "1" serves MCP over stdio; "fail"
// writes to stderr and exits non-zero without speaking MCP.
const helperEnvKey = "GO_MCPCLI_CONNECTION_HELPER"

// helperStderrMessage is what the helper subprocess prints to stderr in
// "fail" mode; TestOpenStdioSubprocessDiesWithStderr asserts it reaches the
// Open error.
const helperStderrMessage = "helper: intentional stdio failure"

// TestHelperProcess acts as an MCP stdio server subprocess when the test
// binary is exec'd by TestOpenStdio with helperEnvKey=1, or as a subprocess
// that dies with a stderr diagnostic when the value is "fail". It returns
// immediately in a normal test run.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnvKey) {
	case "1":
		srv := mcp.NewServer(&mcp.Implementation{Name: "stdio-test-server", Version: "0.9.0"}, nil)
		// Run blocks until the client closes the connection (stdin EOF).
		if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			t.Errorf("helper stdio server: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	case "fail":
		fmt.Fprintln(os.Stderr, helperStderrMessage)
		os.Exit(3)
	}
}

func TestOpenStdio(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error: %v", err)
	}
	// The child runs only TestHelperProcess and inherits helperEnvKey from
	// this process's environment, turning it into a stdio MCP server.
	t.Setenv(helperEnvKey, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := Open(ctx, &Options{Command: shellquote.Join(exe, "-test.run=TestHelperProcess"), ProtocolVersion: "auto"})
	if err != nil {
		t.Fatalf("Open() stdio error: %v", err)
	}
	defer session.Close()

	if err := session.Ping(ctx, nil); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}

	res := session.InitializeResult()
	if res.ServerInfo.Name != "stdio-test-server" || res.ServerInfo.Version != "0.9.0" {
		t.Fatalf("InitializeResult().ServerInfo = %s/%s, want stdio-test-server/0.9.0", res.ServerInfo.Name, res.ServerInfo.Version)
	}
}

func TestOpenStdioSubprocessDiesWithStderr(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error: %v", err)
	}
	// The child runs only TestHelperProcess, which writes a diagnostic to
	// stderr and exits non-zero without speaking MCP — the realistic bad
	// command (wrong interpreter, missing module, bad script) where the
	// process starts but the connect fails.
	t.Setenv(helperEnvKey, "fail")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := Open(ctx, &Options{Command: shellquote.Join(exe, "-test.run=TestHelperProcess"), ProtocolVersion: "auto"})
	if err == nil {
		if session != nil {
			session.Close()
		}
		t.Fatal("Open() with a dying subprocess = nil error, want failure")
	}
	if session != nil {
		session.Close()
	}
	if !strings.Contains(err.Error(), helperStderrMessage) {
		t.Fatalf("Open() error %q does not include subprocess stderr %q", err.Error(), helperStderrMessage)
	}
}

func TestOpenStdioBadCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const missing = "definitely-not-a-real-binary-xyz"
	session, err := Open(ctx, &Options{Command: missing, ProtocolVersion: "auto"})
	if err == nil {
		session.Close()
		t.Fatal("Open() with a missing binary = nil error, want exec failure")
	}
	if session != nil {
		session.Close()
	}
	// The SDK wraps exec errors; require the message to identify the failure.
	msg := err.Error()
	if !strings.Contains(msg, missing) &&
		!strings.Contains(msg, "executable file not found") &&
		!strings.Contains(msg, "no such file") {
		t.Fatalf("Open() error %q does not mention the binary or the exec failure", msg)
	}
}

// TestSplitCommandQuoting pins the shell-like quoting the --command flag
// documents, which transportFor relies on shellquote.Split to honor.
func TestSplitCommandQuoting(t *testing.T) {
	argv, err := shellquote.Split(`npx -y "some server" --port 8080`)
	if err != nil {
		t.Fatalf("Split() error: %v", err)
	}
	want := []string{"npx", "-y", "some server", "--port", "8080"}
	if !slices.Equal(argv, want) {
		t.Fatalf("Split() = %q, want %q", argv, want)
	}

	if _, err := shellquote.Split(`prog "unbalanced`); err == nil {
		t.Fatal("Split() with an unbalanced quote = nil error, want error")
	}
}
