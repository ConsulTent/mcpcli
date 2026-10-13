package cli

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// newInfoCommand builds "info": report what the server declared at initialize
// — name, version, negotiated protocol version, instructions, capabilities —
// as human-readable lines, or the raw InitializeResult object in --json mode.
func newInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show the server's identity, protocol version, and capabilities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return opInfo(session, cmd.OutOrStdout())
			})
		},
	}
}
