package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/stephenrhoton/micro-status-mcp/internal/server"
)

func newListCommand() *cobra.Command {
	var (
		repo        string
		includeRead bool
		limit       int
		sinceID     int64
		omitBody    bool
		jsonOut     bool
		markRead    bool
		endpoint    string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List messages addressed to a repo.",
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

			var out server.ListMessagesResult
			args := server.ListMessagesParams{
				Repo: repo, IncludeRead: includeRead,
				Limit: limit, SinceID: sinceID, OmitBody: omitBody,
			}
			if _, err := callToolTyped(ctx, sess, "list_messages", args, &out); err != nil {
				return err
			}

			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(out)
			}

			if len(out.Messages) == 0 {
				fmt.Printf("no messages for %s\n", repo)
				return nil
			}

			for _, m := range out.Messages {
				readMarker := ""
				if m.ReadAt != nil {
					readMarker = " [read]"
				}
				fmt.Printf("#%d  %s -> %s  %s%s\n",
					m.ID, m.From, m.To, m.PostedAt.Format(time.RFC3339), readMarker)
				fmt.Printf("    subject: %s\n", m.Subject)
				if m.Body != "" {
					fmt.Printf("    body: %s\n", m.Body)
				}
			}

			if markRead {
				ids := make([]int64, 0, len(out.Messages))
				for _, m := range out.Messages {
					if m.ReadAt == nil {
						ids = append(ids, m.ID)
					}
				}
				if len(ids) == 0 {
					fmt.Println("(no unread messages to mark)")
					return nil
				}
				var res server.MarkReadBulkResult
				if _, err := callToolTyped(ctx, sess, "mark_read_bulk",
					server.MarkReadBulkParams{IDs: ids}, &res); err != nil {
					return fmt.Errorf("mark_read_bulk: %w", err)
				}
				fmt.Printf("(%d message(s) marked read)\n", res.Updated)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "repo identifier")
	cmd.Flags().BoolVar(&includeRead, "include-read", false, "include messages already marked read")
	cmd.Flags().IntVar(&limit, "limit", 0, "max messages to return (default 100, max 1000)")
	cmd.Flags().Int64Var(&sinceID, "since-id", 0, "return only messages with id > this value")
	cmd.Flags().BoolVar(&omitBody, "omit-body", false, "do not include message bodies in the response")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of human-readable output")
	cmd.Flags().BoolVar(&markRead, "mark-read", false, "mark every listed message as read after printing")
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultEndpoint, "MCP server endpoint")
	return cmd
}

func newSessionsCommand() *cobra.Command {
	var (
		jsonOut  bool
		endpoint string
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Show currently registered sessions and their tmux panes.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			sess, err := connect(ctx, endpoint)
			if err != nil {
				return err
			}
			defer func() { _ = sess.Close() }()

			var out server.ListSessionsResult
			if _, err := callToolTyped(ctx, sess, "list_sessions", server.ListSessionsParams{}, &out); err != nil {
				return err
			}

			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(out)
			}

			if len(out.Sessions) == 0 {
				fmt.Println("no registered sessions")
				return nil
			}
			for _, s := range out.Sessions {
				fmt.Printf("%-30s  %s  (since %s)\n", s.Repo, s.Pane, s.RegisteredAt.Format(time.RFC3339))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of human-readable output")
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultEndpoint, "MCP server endpoint")
	return cmd
}
