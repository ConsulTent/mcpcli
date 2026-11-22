package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/connection"
	"github.com/ConsulTent/mcpcli/internal/trace"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// insecureHTTPClient returns an http.Client whose transport is a Clone of
// http.DefaultTransport with TLSClientConfig.InsecureSkipVerify set — the
// --insecure recipe every HTTP path of a run shares (tus builds the same
// client inside its own package, connection.go inside its own). A Clone
// preserves the proxy environment and HTTP/2 that a bare &http.Transport{}
// would silently drop, and proxy + self-signed-MITM is exactly the
// --insecure demographic.
func insecureHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.InsecureSkipVerify = true
	return &http.Client{Transport: t}
}

// runWithConnection is the shared runner for every command that talks to an
// MCP server. It requires exactly one target (--command XOR --url, enforced
// before anything else so failures need no network), resolves the auth
// provider from the registry (no mode-name logic here), and opens the session
// under the ui spinner. One --timeout window bounds the whole operation: the
// OAuth wait inside Build, the connect, and fn itself. The auth cleanup and
// the session are released when fn returns; fn's error propagates unchanged
// (callers keep their own semantics — notably format.ErrToolError passes
// through and cobra's exit path turns any error into exit 1).
//
// errOut carries decoration: the --client-secret-style warnings and, under
// --trace, the timing report (the op* pattern — callers pass
// cmd.ErrOrStderr()). Under --trace the connect spinner is replaced by the
// report's own timings (--verbose adds the totals line); --verbose implies
// --trace.
func runWithConnection(ctx context.Context, f *GlobalFlags, errOut io.Writer, fn func(context.Context, *mcp.ClientSession) error) error {
	if f == nil {
		return errors.New("global flags unavailable: command must be built by NewRootCommand")
	}
	// Validate the target shape up front, reusing the connection package's
	// exact messages. AuthHandler is not set yet, so this checks exactly the
	// --command XOR --url rule; the stdio-rejects-auth rule still fires inside
	// Open once a handler exists.
	if err := (&connection.Options{Command: f.Command, URL: f.URL}).Validate(); err != nil {
		return err
	}
	target := f.URL
	if target == "" {
		target = f.Command
	}

	var tracer *trace.Tracer
	if f.Trace || f.Verbose {
		tracer = trace.New(f.Verbose)
	}

	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()

	// --insecure: the TLS-skip client rides the context from here on, so
	// every oauth/client-credentials discovery fetch inside the SDK
	// contract (oauth2.NewClient(ctx, nil)) skips verification too. The
	// connection layer sets transport.HTTPClient itself from
	// Options.Insecure — ctx injection alone covers only discovery, not
	// the transport's own wire requests.
	if f.Insecure {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, insecureHTTPClient())
	}

	// Secret references resolve before anything connects: the expanded
	// client secret feeds auth.Params below, and the env map (stdio runs
	// only) rides both connection.Options constructions.
	env, err := expandSecretRefs(ctx, f, errOut)
	if err != nil {
		return err
	}

	buildStart := time.Now()
	provider, err := auth.ProviderFor(f.Auth)
	if err != nil {
		return err
	}
	handler, cleanup, err := provider.Build(ctx, auth.Params{
		Target:   target,
		Creds:    auth.ResolveCredentials(f.ClientID, f.ClientSecret),
		Stderr:   os.Stderr,
		Stdin:    os.Stdin,
		Flow:     f.AuthFlow,
		Resource: f.Resource,
	})
	tracer.SetPhase("build", time.Since(buildStart))
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	openStart := time.Now()
	var session *mcp.ClientSession
	if tracer != nil {
		// No spinner under trace: the report's boot/open rows carry the same
		// numbers without printing them twice.
		session, err = connection.Open(ctx, &connection.Options{
			Command:         f.Command,
			URL:             f.URL,
			Auth:            f.Auth,
			ProtocolVersion: f.Protocol,
			AuthHandler:     handler,
			Env:             env,
			Insecure:        f.Insecure,
			Tracer:          tracer,
			Label:           "connect",
		})
	} else {
		err = ui.RunWithSpinner(ctx, "connecting to "+target, func(ctx context.Context) error {
			session, err = connection.Open(ctx, &connection.Options{
				Command:         f.Command,
				URL:             f.URL,
				Auth:            f.Auth,
				ProtocolVersion: f.Protocol,
				AuthHandler:     handler,
				Env:             env,
				Insecure:        f.Insecure,
			})
			return err
		})
	}
	tracer.SetPhase("open", time.Since(openStart))
	if err != nil {
		// A successful Open followed by a spinner failure (e.g. the Rich
		// program's own error) still leaves an open session: close it.
		if session != nil {
			_ = session.Close()
		}
		return err
	}
	defer session.Close()

	opStart := time.Now()
	fnErr := fn(ctx, session)
	tracer.SetPhase("op", time.Since(opStart))
	if tracer != nil {
		if ui.Current == ui.JSON {
			_ = tracer.EmitJSON(errOut)
		} else {
			_ = tracer.Render(errOut, ui.Current == ui.Plain, f.Verbose)
		}
	}
	return fnErr
}
