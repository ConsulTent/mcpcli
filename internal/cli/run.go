package cli

import (
	"context"
	"errors"
	"os"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/connection"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runWithConnection is the shared runner for every command that talks to an
// MCP server. It requires exactly one target (--command XOR --url, enforced
// before anything else so failures need no network), resolves the auth
// provider from the registry (no mode-name logic here), and opens the session
// under the ui spinner. One --timeout window bounds the whole operation: the
// OAuth wait inside Build, the connect, and fn itself. The auth cleanup and
// the session are released when fn returns; fn's error propagates unchanged
// (callers keep their own semantics — notably format.ErrToolError passes
// through and cobra's exit path turns any error into exit 1).
func runWithConnection(ctx context.Context, f *GlobalFlags, fn func(context.Context, *mcp.ClientSession) error) error {
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

	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()

	provider, err := auth.ProviderFor(f.Auth)
	if err != nil {
		return err
	}
	handler, cleanup, err := provider.Build(ctx, auth.Params{
		Target: target,
		Creds:  auth.ResolveCredentials(f.ClientID, f.ClientSecret),
		Stderr: os.Stderr,
		Stdin:  os.Stdin,
		Flow:   f.AuthFlow,
	})
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	var session *mcp.ClientSession
	err = ui.RunWithSpinner(ctx, "connecting to "+target, func(ctx context.Context) error {
		session, err = connection.Open(ctx, &connection.Options{
			Command:         f.Command,
			URL:             f.URL,
			Auth:            f.Auth,
			ProtocolVersion: f.Protocol,
			AuthHandler:     handler,
		})
		return err
	})
	if err != nil {
		// A successful Open followed by a spinner failure (e.g. the Rich
		// program's own error) still leaves an open session: close it.
		if session != nil {
			_ = session.Close()
		}
		return err
	}
	defer session.Close()

	return fn(ctx, session)
}
