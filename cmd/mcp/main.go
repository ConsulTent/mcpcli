// Command mcp is an MCP (Model Context Protocol) client and inspector.
package main

import (
	"os"

	"github.com/ConsulTent/mcpcli/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
