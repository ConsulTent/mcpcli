package cli

import (
	"context"
	"io"
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
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), cmd.ErrOrStderr(), func(ctx context.Context, session *mcp.ClientSession) error {
				return opListTools(ctx, session, cmd.OutOrStdout())
			})
		},
	}
}

// newToolsCallCommand builds "tools call NAME": run one tool and print its
// result. Arguments come from --arg KEY=VALUE pairs, a -A/--arg-file JSON
// object, and --in-file [KEY=]PATH raw-text files (loaded in that order;
// later sources override per key). An isError result is a command failure
// (exit 1) in both output modes — in --json mode the full CallToolResult
// still reaches stdout first.
func newToolsCallCommand() *cobra.Command {
	var (
		pairs   []string
		argFile string
		outfile string
		inFiles []string
	)
	cmd := &cobra.Command{
		Use:   "call NAME",
		Short: "Call a tool by name with the given arguments",
		Long: `Call a tool by name with its arguments.

Arguments come from repeatable --arg KEY=VALUE pairs, a -A/--arg-file JSON
object ("-" reads stdin), and repeatable --in-file [KEY=]PATH raw-text
files ("-" reads stdin, once per invocation). They load in that order, so
later sources override earlier ones per key; repeated keys take the last
value. --arg values are JSON-decoded when they parse as JSON (count=3 is
the number 3, x={"a":1} an object); --in-file values are NEVER decoded —
the file's content becomes the argument as a raw string, which is the
point: large or quoted text goes file-to-wire untouched. Each --in-file
read is capped at 8 MiB and must be valid UTF-8; tools call has no primary
text input, so a bare PATH (no KEY=) is an error here.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parse and merge the arguments before any connection is opened,
			// so malformed input fails fast without touching the network.
			toolArgs, err := buildToolArgs(argFile, pairs, inFiles, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), cmd.ErrOrStderr(), func(ctx context.Context, session *mcp.ClientSession) error {
				return opCallTool(ctx, session, args[0], toolArgs, cmd.OutOrStdout(), cmd.ErrOrStderr(), outfile)
			})
		},
	}
	cmd.Flags().StringArrayVar(&pairs, "arg", nil, "tool argument as KEY=VALUE; value is JSON-decoded when it parses as JSON, else a raw string (repeatable, last wins)")
	cmd.Flags().StringVarP(&argFile, "arg-file", "A", "", `JSON object of tool arguments from FILE ("-" for stdin); --arg pairs override it`)
	cmd.Flags().StringArrayVar(&inFiles, "in-file", nil, `raw-string text argument from FILE as KEY=PATH ("-" for stdin read once; repeatable; loads after --arg-file, before --arg; 8 MiB cap, must be UTF-8)`)
	cmd.Flags().StringVar(&outfile, "outfile", "", "write image/audio content to FILE; extension from MIME type, numbered NAME.N.EXT when multiple")
	return cmd
}

// buildToolArgs merges the argument sources in precedence order: the -A
// JSON object first, then --in-file raw-text values, then --arg pairs (which
// override everything per key). in-file reads print "read …" notices to
// errOut in every output mode.
func buildToolArgs(argFile string, pairs, inFiles []string, errOut io.Writer) (map[string]any, error) {
	toolArgs := map[string]any{}
	if argFile != "" {
		fileArgs, err := loadArgsFile(argFile)
		if err != nil {
			return nil, err
		}
		maps.Copy(toolArgs, fileArgs)
	}
	if len(inFiles) > 0 {
		specs, err := parseInFiles(inFiles, nil, false)
		if err != nil {
			return nil, err
		}
		_, values, err := applyInFiles(specs, nil, errOut)
		if err != nil {
			return nil, err
		}
		for _, v := range values {
			toolArgs[v.Key] = v.Content
		}
	}
	pairArgs, err := parseToolArgs(pairs)
	if err != nil {
		return nil, err
	}
	maps.Copy(toolArgs, pairArgs)
	return toolArgs, nil
}
