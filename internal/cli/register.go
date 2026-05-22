package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/stephenrhoton/micro-status-mcp/internal/server"
)

func newRegisterCommand() *cobra.Command {
	var (
		repo     string
		pane     string
		endpoint string
	)

	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register this Claude session's tmux pane with the running MCP server.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" || pane == "" {
				return fmt.Errorf("--repo and --pane are required")
			}
			ctx := cmd.Context()
			sess, err := connect(ctx, endpoint)
			if err != nil {
				return err
			}
			defer func() { _ = sess.Close() }()

			var out server.RegisterResult
			args := server.RegisterParams{Repo: repo, Pane: pane}
			if _, err := callToolTyped(ctx, sess, "register", args, &out); err != nil {
				return err
			}
			if out.PreviousPane != "" && out.PreviousPane != out.Pane {
				fmt.Printf("registered %s -> %s (replaced %s)\n", out.Repo, out.Pane, out.PreviousPane)
			} else {
				fmt.Printf("registered %s -> %s\n", out.Repo, out.Pane)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "repo identifier (typically basename of $PWD)")
	cmd.Flags().StringVar(&pane, "pane", "", "tmux target in session:window.pane form")
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultEndpoint, "MCP server endpoint")
	return cmd
}

func newUnregisterCommand() *cobra.Command {
	var (
		repo     string
		endpoint string
	)
	cmd := &cobra.Command{
		Use:   "unregister",
		Short: "Remove this repo's pane registration.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repo == "" {
				return fmt.Errorf("--repo is required")
			}
			ctx := cmd.Context()
			sess, err := connect(ctx, endpoint)
			if err != nil {
				return err
			}
			defer func() { _ = sess.Close() }()

			var out server.UnregisterResult
			args := server.UnregisterParams{Repo: repo}
			if _, err := callToolTyped(ctx, sess, "unregister", args, &out); err != nil {
				return err
			}
			fmt.Printf("unregistered %s (removed=%t)\n", repo, out.Removed)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repo identifier")
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultEndpoint, "MCP server endpoint")
	return cmd
}
