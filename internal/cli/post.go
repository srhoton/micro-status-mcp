package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/stephenrhoton/micro-status-mcp/internal/server"
)

func newPostCommand() *cobra.Command {
	var (
		from     string
		to       string
		subject  string
		body     string
		bodyFile string
		endpoint string
	)

	cmd := &cobra.Command{
		Use:   "post",
		Short: "Post a message from one repo to another.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" || to == "" || subject == "" {
				return fmt.Errorf("--from, --to, and --subject are required")
			}

			resolvedBody, err := resolveBody(body, bodyFile, os.Stdin)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			sess, err := connect(ctx, endpoint)
			if err != nil {
				return err
			}
			defer func() { _ = sess.Close() }()

			var out server.PostMessageResult
			args := server.PostMessageParams{
				From: from, To: to, Subject: subject, Body: resolvedBody,
			}
			if _, err := callToolTyped(ctx, sess, "post_message", args, &out); err != nil {
				return err
			}
			line := fmt.Sprintf("posted id=%d notified=%t", out.ID, out.Notified)
			if out.NotifyError != "" {
				line += fmt.Sprintf(" notify_error=%q", out.NotifyError)
			}
			fmt.Println(line)
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "sender repo")
	cmd.Flags().StringVar(&to, "to", "", "recipient repo")
	cmd.Flags().StringVar(&subject, "subject", "", "subject line")
	cmd.Flags().StringVar(&body, "body", "", "message body (use --body-file for stdin/file)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read body from this file ('-' for stdin)")
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultEndpoint, "MCP server endpoint")
	return cmd
}

// resolveBody chooses between inline --body, --body-file path, and stdin
// (--body-file -). It rejects mutually exclusive combinations and enforces
// the server's MaxBodyBytes cap on input.
func resolveBody(inline, bodyFile string, stdin io.Reader) (string, error) {
	if inline != "" && bodyFile != "" {
		return "", fmt.Errorf("--body and --body-file are mutually exclusive")
	}
	if inline != "" {
		if len(inline) > server.MaxBodyBytes {
			return "", fmt.Errorf("body too large: %d bytes (max %d)", len(inline), server.MaxBodyBytes)
		}
		return inline, nil
	}
	switch bodyFile {
	case "":
		return "", nil
	case "-":
		return readCapped(stdin, "<stdin>")
	default:
		f, err := os.Open(bodyFile)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", bodyFile, err)
		}
		defer func() { _ = f.Close() }()
		return readCapped(f, bodyFile)
	}
}

// readCapped reads at most MaxBodyBytes+1 bytes from r and returns an error
// when the input would exceed MaxBodyBytes, preventing unbounded allocations
// for accidental --body-file /dev/zero invocations.
func readCapped(r io.Reader, name string) (string, error) {
	limited := io.LimitReader(r, int64(server.MaxBodyBytes)+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	if len(buf) > server.MaxBodyBytes {
		return "", fmt.Errorf("body from %s too large (max %d bytes)", name, server.MaxBodyBytes)
	}
	return string(buf), nil
}
