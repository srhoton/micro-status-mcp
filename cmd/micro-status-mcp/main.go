// Command micro-status-mcp is a long-lived MCP server (plus a thin CLI) that
// brokers messages between Claude Code sessions running in different repos.
package main

import (
	"fmt"
	"os"

	"github.com/stephenrhoton/micro-status-mcp/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
