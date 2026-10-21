package cli

import (
	"context"
	"maps"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// newToolsCommand builds the "tools" group: list what the server exposes and
// call a tool by name.
func newToolsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "List or call the server's tools",
		// Runnable (printing help, exactly what cobra does for a bare group)
		// so the root's PersistentPreRunE runs here too — notably its
		// --interactive check, which must reject "-i tools" instead of
		// silently printing help.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newToolsListCommand(), newToolsCallCommand())
	return cmd
}

// newToolsListCommand builds "tools list": the server's tools as a name and
// description table, or the raw ListToolsResult in --json mode. An empty list
// is a valid, successful result.
func newToolsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the tools the server exposes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return opListTools(ctx, session, cmd.OutOrStdout())
			})
		},
	}
}

// newToolsCallCommand builds "tools call NAME": run one tool and print its
// result. Arguments come from --arg KEY=VALUE pairs and a -A/--arg-file JSON
// object; the file loads first and the pairs override it key by key. An isError
// result is a command failure (exit 1) in both output modes — in --json mode
// the full CallToolResult still reaches stdout first.
func newToolsCallCommand() *cobra.Command {
	var (
		pairs   []string
		argFile string
		outfile string
	)
	cmd := &cobra.Command{
		Use:   "call NAME",
		Short: "Call a tool by name with the given arguments",
		Long: `Call a tool by name with the given arguments.

Arguments come from repeatable --arg KEY=VALUE pairs and, optionally, a
JSON object loaded with -A/--arg-file FILE ("-" reads stdin). File
arguments load first and --arg pairs override them key by key; repeated
--arg keys take the last value. --arg values are JSON-decoded when they
parse as JSON (count=3 is the number 3, x={"a":1} an object) and kept
as raw strings otherwise.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parse and merge the arguments before any connection is opened,
			// so malformed input fails fast without touching the network.
			toolArgs, err := buildToolArgs(argFile, pairs)
			if err != nil {
				return err
			}
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return opCallTool(ctx, session, args[0], toolArgs, cmd.OutOrStdout(), cmd.ErrOrStderr(), outfile)
			})
		},
	}
	cmd.Flags().StringArrayVar(&pairs, "arg", nil, "tool argument as KEY=VALUE; value is JSON-decoded when it parses as JSON, else a raw string (repeatable, last wins)")
	cmd.Flags().StringVarP(&argFile, "arg-file", "A", "", `JSON object of tool arguments from FILE ("-" for stdin); --arg pairs override it`)
	cmd.Flags().StringVar(&outfile, "outfile", "", "write image/audio content to FILE; extension from MIME type, numbered NAME.N.EXT when multiple")
	return cmd
}

// buildToolArgs merges the args file (loaded first) with the --arg pairs
// (applied last, so they override per key).
func buildToolArgs(argFile string, pairs []string) (map[string]any, error) {
	toolArgs := map[string]any{}
	if argFile != "" {
		fileArgs, err := loadArgsFile(argFile)
		if err != nil {
			return nil, err
		}
		maps.Copy(toolArgs, fileArgs)
	}
	pairArgs, err := parseToolArgs(pairs)
	if err != nil {
		return nil, err
	}
	maps.Copy(toolArgs, pairArgs)
	return toolArgs, nil
}
