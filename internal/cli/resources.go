package cli

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// newResourcesCommand builds the "resources" group: list what the server
// exposes and read one resource's contents.
func newResourcesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resources",
		Short: "List or read the server's resources",
		// Runnable (printing help, exactly what cobra does for a bare group)
		// so the root's PersistentPreRunE runs here too — notably its
		// --interactive check.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newResourcesListCommand(), newResourcesReadCommand())
	return cmd
}

// newResourcesListCommand builds "resources list": the server's static
// resources and URI templates as tables, or both list results as one JSON
// document in --json mode.
func newResourcesListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the resources and resource templates the server exposes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return opListResources(ctx, session, cmd.OutOrStdout())
			})
		},
	}
}

// newResourcesReadCommand builds "resources read URI": fetch one resource and
// print its contents, or the raw ReadResourceResult in --json mode. With
// --outfile, blob contents divert to files while text contents still print.
func newResourcesReadCommand() *cobra.Command {
	var outfile string
	cmd := &cobra.Command{
		Use:   "read URI",
		Short: "Read a resource by URI and print its contents",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return opReadResource(ctx, session, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr(), outfile)
			})
		},
	}
	cmd.Flags().StringVar(&outfile, "outfile", "", "write binary (blob) content to FILE; extension from MIME type, numbered NAME.N.EXT when multiple")
	return cmd
}
