// Package cli builds the cobra command tree for the micro-status-mcp binary.
package cli

import "github.com/spf13/cobra"

// Version is set by main; default to "dev".
var Version = "dev"

const (
	defaultAddr = "127.0.0.1:7878"
	mcpPath     = "/mcp"
)

// defaultEndpoint is derived from defaultAddr + mcpPath so the two cannot
// drift independently.
var defaultEndpoint = "http://" + defaultAddr + mcpPath

// NewRootCommand wires up every subcommand.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "micro-status-mcp",
		Short:         "MCP-based agent mailbox for Claude Code sessions",
		Long:          "micro-status-mcp runs a local MCP server that lets Claude sessions in different repos message each other and wake each other via tmux send-keys.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newServeCommand())
	root.AddCommand(newRegisterCommand())
	root.AddCommand(newUnregisterCommand())
	root.AddCommand(newPostCommand())
	root.AddCommand(newListCommand())
	root.AddCommand(newSessionsCommand())

	return root
}
