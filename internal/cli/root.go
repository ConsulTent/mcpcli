// Package cli assembles the mcp command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/ConsulTent/mcpcli/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// init populates the auth provider registry before any command runs, so
// that --auth validation in PersistentPreRunE consults the live registry
// rather than a hardcoded mode list.
func init() {
	auth.RegisterBuiltins()
}

// GlobalFlags holds the values of the persistent flags shared by every
// mcp subcommand. NewRootCommand binds the flags into it.
type GlobalFlags struct {
	Command      string
	URL          string
	Auth         string
	AuthFlow     string
	Protocol     string
	ClientID     string
	ClientSecret string
	Timeout      time.Duration
	JSON         bool
	Plain        bool
	Interactive  bool
}

// NewRootCommand builds the mcp root command with all persistent flags
// bound into a fresh GlobalFlags value. The flags pointer is stored on the
// root command's context, which cobra passes to every executed subcommand, so
// commands reach it with FlagsFrom without NewRootCommand exposing it
// directly.
func NewRootCommand() *cobra.Command {
	g := &GlobalFlags{}

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP client and inspector",
		Long: `mcp is a command-line MCP (Model Context Protocol) client and inspector.
It connects to any MCP server over stdio or streamable HTTP to inspect its
identity, capabilities, tools, resources, and prompts, call tools, probe
protocol-version compatibility, and work interactively in a REPL.

Website: https://consultent.ltd
Copyright ConsulTent Pte. Ltd., all Rights Reserved.`,
		Example: `  mcp --command "npx -y @modelcontextprotocol/server-everything" tools list
  mcp --url https://example.com/mcp info
  mcp compat --url https://example.com/mcp`,
		SilenceUsage: true,
		Version:      version.Current(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !g.Interactive {
				return cmd.Help()
			}
			return runWithConnection(cmd.Context(), g, func(ctx context.Context, session *mcp.ClientSession) error {
				// runWithConnection bounds its whole run -- OAuth wait,
				// connect, and fn -- with one --timeout window, but a REPL
				// must outlive that connect window. WithoutCancel drops the
				// deadline (keeping the context values); runRepl re-bounds
				// every command with its own --timeout window instead.
				return runRepl(context.WithoutCancel(ctx), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), session, g)
			})
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Interactive mode is a whole-run mode: it opens the session and
			// hands it to the REPL, so no subcommand may combine with it
			// (auth login/logout and the rest reject it here, exit 1).
			if g.Interactive && cmd != cmd.Root() {
				return errors.New("--interactive takes no subcommand")
			}
			// --auth-flow is silently inert without --auth oauth, so an
			// explicitly-set combination is rejected rather than ignored —
			// the user asked for a behavior the run will not deliver. The
			// exception is "auth login", which always runs the oauth flow
			// regardless of --auth, so the flag is meaningful there.
			if cmd.Flags().Changed("auth-flow") && g.Auth != "oauth" && cmd.Name() != "login" {
				return errors.New("--auth-flow only applies to --auth oauth")
			}
			// The output layer is global per run: derive it from the output
			// flags before any command emits UI lines.
			switch {
			case g.JSON:
				ui.Current = ui.JSON
			case g.Plain:
				ui.Current = ui.Plain
			default:
				ui.Current = ui.Rich
			}
			// Functional security note, not mode-gated decoration: a secret
			// on the command line is visible in shell history and process
			// listings, so every run of the flag says so on stderr — even in
			// JSON mode, which silences decoration only.
			if g.ClientSecret != "" {
				fmt.Fprintln(os.Stderr, "warning: --client-secret was passed on the command line; prefer the MCPCLI_CLIENT_SECRET environment variable")
			}
			if _, err := auth.ProviderFor(g.Auth); err != nil {
				return fmt.Errorf("invalid value for --auth: %w", err)
			}
			return nil
		},
	}
	cmd.SetContext(context.WithValue(context.Background(), flagsContextKey{}, g))

	pf := cmd.PersistentFlags()
	pf.StringVar(&g.Command, "command", "", `stdio server to launch, e.g. "npx -y @modelcontextprotocol/server-filesystem /tmp"`)
	pf.StringVar(&g.URL, "url", "", "streamable HTTP endpoint of the MCP server")
	pf.StringVar(&g.Auth, "auth", "none", "auth mode: none, bearer, client-credentials, oauth")
	pf.StringVar(&g.AuthFlow, "auth-flow", "loopback", "OAuth flow: loopback or manual (only used with --auth oauth)")
	pf.StringVar(&g.Protocol, "protocol", "auto", `MCP protocol version: auto, draft, or a date such as "2025-06-18"`)
	pf.StringVar(&g.ClientID, "client-id", "", "OAuth client ID (env MCPCLI_CLIENT_ID)")
	pf.StringVar(&g.ClientSecret, "client-secret", "", "OAuth client secret (env MCPCLI_CLIENT_SECRET)")
	pf.DurationVar(&g.Timeout, "timeout", 30*time.Second, "timeout for network operations")
	pf.BoolVar(&g.JSON, "json", false, "emit machine-readable JSON output")
	pf.BoolVar(&g.Plain, "plain", false, "plain ASCII output without emoji, ANSI colors, or spinners")
	pf.BoolVarP(&g.Interactive, "interactive", "i", false, "start the interactive REPL")

	// Subcommands. The auth group manages cached OAuth credentials instead
	// of opening a server session, so it imposes no connection flags.
	// compat opens its own pinned sessions and ignores auth; jev check opens
	// its own session but honors auth.
	cmd.AddCommand(newAuthCommand(g), newInfoCommand(), newPingCommand(), newCompatCommand(), newToolsCommand(), newResourcesCommand(), newPromptsCommand(), newJevCommand())

	return cmd
}

// flagsContextKey is the context key under which NewRootCommand stores the
// shared GlobalFlags.
type flagsContextKey struct{}

// FlagsFrom returns the GlobalFlags NewRootCommand bound, reachable from any
// command in the tree through cobra's propagated context. It returns nil for
// commands built outside NewRootCommand.
func FlagsFrom(cmd *cobra.Command) *GlobalFlags {
	if cmd == nil || cmd.Context() == nil {
		return nil
	}
	g, _ := cmd.Context().Value(flagsContextKey{}).(*GlobalFlags)
	return g
}

// Execute runs the mcp command line using os.Args.
func Execute() error {
	return NewRootCommand().Execute()
}
