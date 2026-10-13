package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// newPromptsCommand builds the "prompts" group: list what the server exposes
// and fetch one prompt with its arguments filled in.
func newPromptsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prompts",
		Short: "List or get the server's prompts",
		// Runnable (printing help, exactly what cobra does for a bare group)
		// so the root's PersistentPreRunE runs here too — notably its
		// --interactive check.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newPromptsListCommand(), newPromptsGetCommand())
	return cmd
}

// newPromptsListCommand builds "prompts list": the server's prompts as a name
// and description table, or the raw ListPromptsResult in --json mode.
func newPromptsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the prompts the server exposes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return opListPrompts(ctx, session, cmd.OutOrStdout())
			})
		},
	}
}

// newPromptsGetCommand builds "prompts get NAME": fetch one prompt with
// --arg KEY=VALUE pairs filling its template arguments. Unlike tools call,
// the values are not JSON-decoded: MCP prompt arguments are string-typed.
func newPromptsGetCommand() *cobra.Command {
	var pairs []string
	cmd := &cobra.Command{
		Use:   "get NAME",
		Short: "Get a prompt by name with the given arguments",
		Long: `Get a prompt by name with the given arguments.

Arguments come from repeatable --arg KEY=VALUE pairs. Prompt
arguments are string-typed per the MCP spec, so values are always
kept as raw strings: count=3 is the string "3", not the number 3.
Repeated --arg keys take the last value.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parse the arguments before any connection is opened, so
			// malformed input fails fast without touching the network.
			promptArgs, err := parsePromptArgs(pairs)
			if err != nil {
				return err
			}
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return opGetPrompt(ctx, session, args[0], promptArgs, cmd.OutOrStdout())
			})
		},
	}
	cmd.Flags().StringArrayVar(&pairs, "arg", nil, "prompt argument as KEY=VALUE; the value is kept as a raw string (repeatable, last wins)")
	return cmd
}

// parsePromptArgs decodes --arg KEY=VALUE pairs into prompt arguments. Each
// pair is split on the first "="; a pair without "=" is an error naming the
// pair. Values stay raw strings — MCP prompt arguments are string-typed, so
// there is no JSON decoding here (unlike parseToolArgs). Repeated keys: last
// wins.
func parsePromptArgs(pairs []string) (map[string]string, error) {
	args := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("invalid argument %q: expected KEY=VALUE", pair)
		}
		args[key] = value
	}
	return args, nil
}
