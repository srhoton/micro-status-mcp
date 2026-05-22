// Package server wires the agent-mailbox MCP tools onto a streamable HTTP
// transport backed by a SQLite store and a tmux notifier.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stephenrhoton/micro-status-mcp/internal/store"
	"github.com/stephenrhoton/micro-status-mcp/internal/tmuxnotify"
)

// DefaultMCPSessionTimeout caps how long an idle MCP session is retained in
// the streamable-HTTP handler's session map before it is evicted, bounding
// the goroutine + heap footprint of clients that crash without sending a
// DELETE.
const DefaultMCPSessionTimeout = 5 * time.Minute

// Notifier is the subset of tmuxnotify.Notifier the server depends on so tests
// can stub the wake-up behavior.
type Notifier interface {
	Notify(ctx context.Context, pane, sender, subject string) error
}

// Service holds the shared dependencies of the MCP tool handlers.
type Service struct {
	Store    *store.Store
	Notifier Notifier
	Logger   *slog.Logger
}

// NewMCPServer constructs an *mcp.Server with all agent-mailbox tools
// registered against svc.
func NewMCPServer(svc *Service, impl *mcp.Implementation) *mcp.Server {
	if impl == nil {
		impl = &mcp.Implementation{Name: "micro-status-mcp", Version: "dev"}
	}
	s := mcp.NewServer(impl, nil)
	svc.register(s)
	return s
}

// HTTPHandler returns a streamable-HTTP handler that serves every request via
// the same shared *mcp.Server returned by NewMCPServer. Idle MCP sessions are
// evicted after DefaultMCPSessionTimeout.
func HTTPHandler(svc *Service, impl *mcp.Implementation) http.Handler {
	mcpServer := NewMCPServer(svc, impl)
	return mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		SessionTimeout: DefaultMCPSessionTimeout,
	})
}

func (svc *Service) logger() *slog.Logger {
	if svc.Logger != nil {
		return svc.Logger
	}
	return slog.Default()
}

// register adds every tool to s.
func (svc *Service) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "register",
		Description: "Register (or update) a Claude session's tmux pane so it can be notified of inbound messages.",
	}, svc.handleRegister)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "unregister",
		Description: "Remove a previously registered session from the pane registry. Idempotent.",
	}, svc.handleUnregister)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "post_message",
		Description: "Post a message to another repo's inbox. If the recipient pane is alive, also wake the recipient via tmux send-keys.",
	}, svc.handlePostMessage)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_messages",
		Description: "List inbound messages for a repo, oldest first. Unread only unless include_read=true.",
	}, svc.handleListMessages)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mark_read",
		Description: "Mark a message as read. Idempotent.",
	}, svc.handleMarkRead)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "mark_read_bulk",
		Description: "Mark multiple messages as read in a single round-trip. Idempotent.",
	}, svc.handleMarkReadBulk)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List currently registered repos and their tmux panes (for discovery / debugging).",
	}, svc.handleListSessions)
}

// pruneCleanupTimeout caps how long the prune-stale-session cleanup may run
// when the original request context has already been canceled.
const pruneCleanupTimeout = 2 * time.Second

// notify is a small helper that runs the notifier and, if the recipient pane
// is confirmed missing, drops the stale registration from the store. Other
// notifier failures (e.g. tmux temporarily unreachable) leave the registration
// in place so it can be retried on the next post.
//
// The cleanup DeleteSession runs on a derived context with its own timeout
// so a client cancellation between the post and the prune does not prevent
// state hygiene.
func (svc *Service) notify(ctx context.Context, repo, pane, sender, subject string) (bool, error) {
	if svc.Notifier == nil {
		return false, nil
	}
	err := svc.Notifier.Notify(ctx, pane, sender, subject)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, tmuxnotify.ErrPaneMissing) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), pruneCleanupTimeout)
		defer cancel()
		// DeleteSessionByPane scopes the delete to the exact pane we just
		// failed to notify. If the recipient has already re-registered with
		// a different pane in the meantime, we leave the new registration
		// alone.
		removed, derr := svc.Store.DeleteSessionByPane(cleanupCtx, repo, pane)
		switch {
		case derr != nil:
			svc.logger().Warn("prune stale session", "repo", repo, "err", derr)
		case removed:
			svc.logger().Info("pruned stale pane registration", "repo", repo, "pane", pane)
		}
		return false, nil
	}
	// Includes ErrTmuxUnavailable: keep the registration; surface the error.
	return false, err
}
