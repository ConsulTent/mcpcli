package connection

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	mcpversion "github.com/ConsulTent/mcpcli/internal/version"
	"github.com/kballard/go-shellquote"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options describes how to reach an MCP server: either a stdio command or a
// streamable HTTP endpoint, plus protocol version pinning and auth.
type Options struct {
	Command string // shell-like argv string to launch over stdio
	URL     string // streamable HTTP endpoint of the MCP server

	Auth            string // auth mode: none, bearer, client-credentials, oauth
	ProtocolVersion string // auto, draft, or a date; see ResolveVersion

	// AuthHandler authorizes streamable HTTP requests. The cli layer builds
	// it from internal/auth; stdio connections reject it because a spawned
	// subprocess has no HTTP surface to authorize.
	AuthHandler sdkauth.OAuthHandler
}

// Validate checks that exactly one transport target is set and that OAuth is
// not combined with stdio.
func (o *Options) Validate() error {
	switch {
	case o.Command != "" && o.URL != "":
		return fmt.Errorf("--command and --url are mutually exclusive")
	case o.Command != "" && o.AuthHandler != nil:
		return fmt.Errorf("--auth is not supported for stdio servers")
	case o.Command == "" && o.URL == "":
		return fmt.Errorf("one of --command or --url is required")
	}
	return nil
}

// Open validates opts, assembles the transport (stdio command or streamable
// HTTP), and connects an mcp client, returning the initialized session.
// When the resolved protocol version is non-empty it is pinned via
// mcp.ClientSessionOptions.ProtocolVersion. A failed stdio connect is
// enriched with the subprocess's captured stderr.
func Open(ctx context.Context, o *Options) (*mcp.ClientSession, error) {
	if o == nil {
		return nil, fmt.Errorf("connection: nil options")
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	version, err := ResolveVersion(o.ProtocolVersion)
	if err != nil {
		return nil, err
	}

	transport, stdio, err := transportFor(o)
	if err != nil {
		return nil, err
	}

	// mcpversion is the internal/version package, aliased because Open's
	// local `version` (the resolved protocol version) shadows the plain name.
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp", Version: mcpversion.Current()}, nil)
	var sessionOpts *mcp.ClientSessionOptions
	if version != "" {
		sessionOpts = &mcp.ClientSessionOptions{ProtocolVersion: version}
	}
	session, err := client.Connect(ctx, transport, sessionOpts)
	if err == nil {
		return session, nil
	}
	if stdio != nil {
		return nil, stdio.wrapConnectError(err)
	}
	// The SDK exposes no typed error for an unauthorized connect, so the
	// heuristic matches the status in the wire text ("Unauthorized", "401").
	// The hint only fires for --auth none: a run that already asked for auth
	// must not be told to add more.
	if o.Auth == "none" && isUnauthorizedConnect(err) {
		return nil, fmt.Errorf("%w; server requires authorization; retry with --auth bearer or --auth oauth", err)
	}
	return nil, err
}

// isUnauthorizedConnect reports whether a connect error looks like an HTTP
// 401/403: the SDK renders the status as its text ("Unauthorized") or the
// bare code, depending on how far the handshake got.
func isUnauthorizedConnect(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "forbidden")
}

// maxStderrBytes bounds the subprocess stderr captured for error diagnostics.
const maxStderrBytes = 4096

// stderrDrainWait bounds how long Open waits for the exec stderr-copy
// goroutine to deliver the child's output after a failed connect.
const stderrDrainWait = time.Second

// stderrBuffer is a bounded io.Writer that keeps the first maxStderrBytes of
// subprocess stderr and discards the rest. Writes always report success so
// the exec copy goroutine never stalls the child on a full buffer. All
// methods are safe for concurrent use: exec copies stderr on its own
// goroutine while Open reads the buffer.
type stderrBuffer struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (b *stderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxStderrBytes - len(b.buf); room > 0 {
		n := min(room, len(p))
		b.buf = append(b.buf, p[:n]...)
		if n < len(p) {
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *stderrBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

func (b *stderrBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return string(b.buf) + "\n...(truncated)"
	}
	return string(b.buf)
}

// stdioCapture carries what Open needs to attach subprocess stderr to a
// failed stdio connect.
type stdioCapture struct {
	cmd    *exec.Cmd
	stderr *stderrBuffer
}

// wrapConnectError enriches a failed stdio connect with the subprocess's
// stderr, so the user sees the actual crash output (missing module, bad
// script, ...) instead of a bare EOF. A failed Start has no subprocess, and
// a quiet child contributes nothing; both keep the original error, with
// context added.
func (s *stdioCapture) wrapConnectError(err error) error {
	if s.cmd.Process != nil { // child actually started: drain its stderr first
		s.awaitStderr()
	}
	if msg := s.stderr.String(); msg != "" {
		return fmt.Errorf("connecting to stdio server: %w\nsubprocess stderr:\n%s", err, msg)
	}
	return fmt.Errorf("connecting to stdio server: %w", err)
}

// awaitStderr waits briefly for the subprocess's stderr to finish draining
// into the buffer: exec copies stderr on its own goroutine, which races with
// the stdout EOF that fails the connect. It returns as soon as anything is
// captured; the deadline only bounds the pathological quiet-child case.
func (s *stdioCapture) awaitStderr() {
	deadline := time.Now().Add(stderrDrainWait)
	for s.stderr.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// transportFor builds the SDK transport described by o, wiring subprocess
// stderr into a bounded capture buffer for stdio. o must have passed
// Validate.
func transportFor(o *Options) (mcp.Transport, *stdioCapture, error) {
	if o.Command != "" {
		argv, err := shellquote.Split(o.Command)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing --command: %w", err)
		}
		if len(argv) == 0 {
			return nil, nil, fmt.Errorf("--command is empty")
		}
		// Resolve the binary here rather than via exec.Command, so a bad
		// --command fails with the flag's value in the message instead of a
		// bare Start error; Args[0] keeps the user's original spelling.
		path, lookErr := exec.LookPath(argv[0])
		if lookErr != nil {
			return nil, nil, fmt.Errorf("resolving --command %q: %w", argv[0], lookErr)
		}
		cmd := &exec.Cmd{Path: path, Args: argv}
		capture := &stdioCapture{cmd: cmd, stderr: &stderrBuffer{}}
		cmd.Stderr = capture.stderr
		return &mcp.CommandTransport{Command: cmd}, capture, nil
	}
	return &mcp.StreamableClientTransport{
		Endpoint:     o.URL,
		OAuthHandler: o.AuthHandler,
	}, nil, nil
}
