package cli

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// newPingCommand builds "ping": time one round trip to the server. Rendering
// (including the mode-specific result line) lives in opPing.
func newPingCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ping",
		Short: "Time a round trip to the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), cmd.ErrOrStderr(), func(ctx context.Context, session *mcp.ClientSession) error {
				return opPing(ctx, session, cmd.OutOrStdout())
			})
		},
	}
}
