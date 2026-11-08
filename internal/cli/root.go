// Package cli assembles the mcp command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
	Env          []string
	Timeout      time.Duration
	JSON         bool
	Plain        bool
	Interactive  bool
	Trace        bool
	Verbose      bool

	// envBindings is Env parsed by PersistentPreRunE — the flag's one parse
	// site: duplicate KEYs collapsed last-wins, surviving pairs in
	// first-appearance order. expandSecretRefs consumes it.
	envBindings []envBinding
}

// envBinding is one parsed --env KEY=VALUE pair.
type envBinding struct {
	Key   string
	Value string
}

// parseEnvPairs parses raw --env pairs: split on the first "=" in the exact
// args.go house error form, empty KEY rejected with the same form, duplicate
// KEYs collapsed last-wins. The root hook is the flag's one parse site; this
// helper is that site's parser.
func parseEnvPairs(pairs []string) ([]envBinding, error) {
	bindings := make([]envBinding, 0, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid argument %q: expected KEY=VALUE", pair)
		}
		replaced := false
		for i := range bindings {
			if bindings[i].Key == key {
				bindings[i].Value = value
				replaced = true
				break
			}
		}
		if !replaced {
			bindings = append(bindings, envBinding{Key: key, Value: value})
		}
	}
	return bindings, nil
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
			return runWithConnection(cmd.Context(), g, cmd.ErrOrStderr(), func(ctx context.Context, session *mcp.ClientSession) error {
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
			// regardless of --auth, so the flag is meaningful there. The
			// parent-group predicate (not the bare command name) keeps
			// "bws login" from slipping through: its --auth-flow would be
			// silently inert too.
			isAuthLogin := cmd.Name() == "login" && cmd.Parent() != nil && cmd.Parent().Name() == "auth"
			if cmd.Flags().Changed("auth-flow") && g.Auth != "oauth" && !isAuthLogin {
				return errors.New("--auth-flow only applies to --auth oauth")
			}
			// --env validation and classification: the flag's one parse
			// site. Malformed input fails on ANY command here — not
			// silently at expansion time on connection commands only.
			resolver, err := newSecretResolver()
			if err != nil {
				return err
			}
			bindings, err := parseEnvPairs(g.Env)
			if err != nil {
				return err
			}
			g.envBindings = bindings
			for _, b := range bindings {
				// A literal VALUE is visible in shell history and process
				// listings, so it says so on stderr — attributing the flag
				// with the KEY (the value itself is never echoed, the
				// --client-secret precedent). References — Recognizes
				// lowercases the scheme, so BWS:// classifies too — and
				// EMPTY values stay silent: an empty injection leaks
				// nothing.
				if b.Value != "" && !resolver.Recognizes(b.Value) {
					// Functional security note, not mode-gated decoration
					// (JSON mode silences decoration only).
					_, _ = fmt.Fprintf(os.Stderr, "warning: --env %s was passed on the command line; prefer a bws:// reference\n", b.Key)
				}
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
			// listings, so every literal run of the flag says so on stderr —
			// even in JSON mode, which silences decoration only. A value
			// that is a registered secret reference (bws:// — Recognizes
			// lowercases the scheme) never reaches argv as plaintext and
			// stays silent.
			if g.ClientSecret != "" && !resolver.Recognizes(g.ClientSecret) {
				_, _ = fmt.Fprintln(os.Stderr, "warning: --client-secret was passed on the command line; prefer bws:// references or MCPCLI_CLIENT_SECRET")
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
	// StringArrayVar, not StringSliceVar: the slice type CSV-splits values
	// and would corrupt a comma-bearing literal (the house repeatable-flag
	// precedent).
	pf.StringArrayVar(&g.Env, "env", nil, "environment variable for stdio servers as KEY=VALUE (repeatable; VALUE may be a bws:// secret reference)")
	pf.DurationVar(&g.Timeout, "timeout", 30*time.Second, "timeout for network operations")
	pf.BoolVar(&g.JSON, "json", false, "emit machine-readable JSON output")
	pf.BoolVar(&g.Plain, "plain", false, "plain ASCII output without emoji, ANSI colors, or spinners")
	pf.BoolVar(&g.Trace, "trace", false, "print a per-call round-trip timing report to stderr (JSON mode: one trace document on stderr; stdout is never touched)")
	pf.BoolVar(&g.Verbose, "verbose", false, "implies --trace and adds process boot, client-internal time, and totals to the report")
	pf.BoolVarP(&g.Interactive, "interactive", "i", false, "start the interactive REPL")

	// Subcommands. The auth group manages cached OAuth credentials instead
	// of opening a server session, so it imposes no connection flags.
	// compat opens its own pinned sessions and ignores auth; jev check opens
	// its own session but honors auth.
	cmd.AddCommand(newAuthCommand(g), newBWSCommand(g), newInfoCommand(), newPingCommand(), newCompatCommand(), newToolsCommand(), newResourcesCommand(), newPromptsCommand(), newJevCommand())

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
