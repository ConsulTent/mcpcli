package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/connection"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/spf13/cobra"
)

// newAuthCommand assembles the "auth" group: login and logout. Neither
// subcommand is a server session command: login performs one throwaway
// connect to trigger the OAuth flow, logout never touches the network.
func newAuthCommand(g *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage cached OAuth credentials",
		// Runnable (printing help, exactly what cobra does for a bare group)
		// so the root's PersistentPreRunE runs here too — notably its
		// --interactive check, which must reject "-i auth login".
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		Long: `Manage cached OAuth credentials.

login runs the MCP OAuth authorization-code flow against a streamable HTTP
server and stores the resulting token in mcp's cache. logout deletes a
cached token without contacting the server.`,
	}
	cmd.AddCommand(newAuthLoginCommand(g), newAuthLogoutCommand(g))
	return cmd
}

// newAuthLoginCommand builds "auth login": a throwaway connect + Ping through
// the normal connection path, whose 401 drives the OAuth provider's
// authorization-code flow and leaves a usable token in the cache.
func newAuthLoginCommand(g *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authorize mcp against a server and cache the OAuth token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()

			// OAuth is a streamable-HTTP flow: the token cache key is the URL.
			if g.Command != "" || g.URL == "" {
				return errors.New("auth login authorizes a streamable HTTP server: pass --url (not --command)")
			}
			// Leaving --auth unset means the oauth flow here; an explicit
			// other mode is a mistake worth catching before connecting.
			if cmd.Flags().Changed("auth") && g.Auth != "oauth" {
				return fmt.Errorf("auth login performs the OAuth flow: pass --auth oauth (got %q)", g.Auth)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), g.Timeout)
			defer cancel()

			provider, err := auth.ProviderFor("oauth")
			if err != nil {
				return err
			}
			handler, cleanup, err := provider.Build(ctx, auth.Params{
				Target: g.URL,
				Creds:  auth.ResolveCredentials(g.ClientID, g.ClientSecret),
				Stderr: errOut,
				Stdin:  cmd.InOrStdin(),
				Flow:   g.AuthFlow,
			})
			if err != nil {
				return err
			}
			defer cleanup()

			ui.Step(errOut, "connecting to %s", g.URL)
			session, err := connection.Open(ctx, &connection.Options{
				URL:             g.URL,
				Auth:            "oauth",
				ProtocolVersion: g.Protocol,
				AuthHandler:     handler,
			})
			if err != nil {
				return err
			}
			defer session.Close()

			// The shared verification helper: meta-aware ping, falling back
			// to the first declared-capability list call on servers without
			// ping. A server with neither still logs in — the OAuth handshake
			// itself is the verification, reported rather than failed.
			via, _, err := verifySession(ctx, session)
			if err != nil {
				if errors.Is(err, errNoVerification) {
					ui.Step(errOut, "server does not implement ping; login verified by connect")
				} else {
					return fmt.Errorf("verifying %s: %w", g.URL, err)
				}
			} else if via != "ping" {
				ui.Step(errOut, "verified via %s", via)
			}
			ui.Success(errOut, "logged in")
			return nil
		},
	}
}

// newAuthLogoutCommand builds "auth logout": delete the cached token for the
// target identified by --url or --command. Strictly offline — the cache key
// is the raw target string, so no connection is made and none is needed.
func newAuthLogoutCommand(g *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete the cached OAuth token for a server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case g.URL != "" && g.Command != "":
				return errors.New("pass only one of --url or --command to auth logout")
			case g.URL == "" && g.Command == "":
				return errors.New("auth logout needs the server to forget: pass --url or --command")
			}
			target := g.URL
			if target == "" {
				target = g.Command
			}
			if err := auth.DeleteToken(target); err != nil {
				return err
			}
			ui.Success(cmd.ErrOrStderr(), "logged out")
			return nil
		},
	}
}
