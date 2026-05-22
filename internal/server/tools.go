package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stephenrhoton/micro-status-mcp/internal/store"
)

// MaxBodyBytes caps a single message body's length on the server side. Posts
// larger than this are rejected before they reach SQLite.
const MaxBodyBytes = 1 << 20 // 1 MiB

// MessageView is the JSON-shaped projection of store.Message used in tool
// outputs.
type MessageView struct {
	ID       int64      `json:"id"`
	From     string     `json:"from"`
	To       string     `json:"to"`
	Subject  string     `json:"subject"`
	Body     string     `json:"body"`
	PostedAt time.Time  `json:"posted_at"`
	ReadAt   *time.Time `json:"read_at,omitempty"`
}

func toMessageView(m store.Message) MessageView {
	return MessageView{
		ID: m.ID, From: m.From, To: m.To,
		Subject: m.Subject, Body: m.Body,
		PostedAt: m.PostedAt, ReadAt: m.ReadAt,
	}
}

// SessionView is the JSON-shaped projection of store.Session.
type SessionView struct {
	Repo         string    `json:"repo"`
	Pane         string    `json:"pane"`
	RegisteredAt time.Time `json:"registered_at"`
}

func toSessionView(s store.Session) SessionView {
	return SessionView{Repo: s.Repo, Pane: s.Pane, RegisteredAt: s.RegisteredAt}
}

// RegisterParams identifies the calling Claude session.
type RegisterParams struct {
	Repo string `json:"repo" jsonschema:"the repo name (basename of the working directory)"`
	Pane string `json:"pane" jsonschema:"the tmux target in session:window.pane form (e.g. main:0.1)"`
}

// RegisterResult reports the new registration and any prior pane it replaced.
type RegisterResult struct {
	Repo         string `json:"repo"`
	Pane         string `json:"pane"`
	PreviousPane string `json:"previous_pane,omitempty"`
}

func (svc *Service) handleRegister(ctx context.Context, _ *mcp.CallToolRequest, params *RegisterParams) (*mcp.CallToolResult, any, error) {
	if err := requireFields(map[string]string{"repo": params.Repo, "pane": params.Pane}); err != nil {
		return nil, nil, err
	}
	prev, saved, err := svc.Store.UpsertSession(ctx, params.Repo, params.Pane)
	if err != nil {
		return nil, nil, fmt.Errorf("register %s: %w", params.Repo, err)
	}
	out := RegisterResult{Repo: saved.Repo, Pane: saved.Pane, PreviousPane: prev}
	text := fmt.Sprintf("registered %s -> %s", out.Repo, out.Pane)
	if prev != "" && prev != saved.Pane {
		text += fmt.Sprintf(" (replaced %s)", prev)
	}
	return textResult(text), out, nil
}

// UnregisterParams names the repo whose registration should be removed.
type UnregisterParams struct {
	Repo string `json:"repo" jsonschema:"the repo to unregister"`
}

// UnregisterResult reports whether a row was actually removed.
type UnregisterResult struct {
	Removed bool `json:"removed"`
}

func (svc *Service) handleUnregister(ctx context.Context, _ *mcp.CallToolRequest, params *UnregisterParams) (*mcp.CallToolResult, any, error) {
	if err := requireFields(map[string]string{"repo": params.Repo}); err != nil {
		return nil, nil, err
	}
	removed, err := svc.Store.DeleteSession(ctx, params.Repo)
	if err != nil {
		return nil, nil, fmt.Errorf("unregister %s: %w", params.Repo, err)
	}
	text := fmt.Sprintf("unregistered %s (removed=%t)", params.Repo, removed)
	return textResult(text), UnregisterResult{Removed: removed}, nil
}

// PostMessageParams is the wire shape for posting a message.
type PostMessageParams struct {
	From    string `json:"from" jsonschema:"the sender repo (basename of caller's working directory)"`
	To      string `json:"to" jsonschema:"the recipient repo"`
	Subject string `json:"subject" jsonschema:"short subject line"`
	Body    string `json:"body" jsonschema:"message body (markdown allowed; capped at 1 MiB)"`
}

// PostMessageResult reports the assigned message ID and whether the recipient
// was woken via tmux send-keys.
type PostMessageResult struct {
	ID          int64  `json:"id"`
	Notified    bool   `json:"notified"`
	NotifyError string `json:"notify_error,omitempty"`
}

func (svc *Service) handlePostMessage(ctx context.Context, _ *mcp.CallToolRequest, params *PostMessageParams) (*mcp.CallToolResult, any, error) {
	if err := requireFields(map[string]string{
		"from": params.From, "to": params.To, "subject": params.Subject,
	}); err != nil {
		return nil, nil, err
	}
	if len(params.Body) > MaxBodyBytes {
		return nil, nil, fmt.Errorf("body too large: %d bytes (max %d)", len(params.Body), MaxBodyBytes)
	}

	m, err := svc.Store.PostMessage(ctx, params.From, params.To, params.Subject, params.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("post_message: %w", err)
	}

	result := PostMessageResult{ID: m.ID}

	sess, err := svc.Store.GetSession(ctx, params.To)
	switch {
	case err == nil:
		notified, nerr := svc.notify(ctx, params.To, sess.Pane, params.From, params.Subject)
		result.Notified = notified
		if nerr != nil {
			result.NotifyError = nerr.Error()
			svc.logger().Warn("notify failed", "to", params.To, "pane", sess.Pane, "err", nerr)
		}
	case errors.Is(err, store.ErrNotFound):
		// Recipient isn't online; message sits in inbox until they call list_messages.
	default:
		svc.logger().Warn("get session for notify", "to", params.To, "err", err)
	}

	text := fmt.Sprintf("posted message id=%d to %s (notified=%t)", result.ID, params.To, result.Notified)
	return textResult(text), result, nil
}

// ListMessagesParams selects which repo's inbox to read.
type ListMessagesParams struct {
	Repo        string `json:"repo" jsonschema:"the recipient repo (basename of caller's working directory)"`
	IncludeRead bool   `json:"include_read,omitempty" jsonschema:"if true, include messages that have already been marked read"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max messages to return (default 100, max 1000)"`
	SinceID     int64  `json:"since_id,omitempty" jsonschema:"if >0, return only messages with id greater than this; use to page forward"`
	OmitBody    bool   `json:"omit_body,omitempty" jsonschema:"if true, return empty body strings so callers can scan subjects without downloading large payloads"`
}

// ListMessagesResult is the list of messages addressed to the repo.
type ListMessagesResult struct {
	Messages []MessageView `json:"messages"`
}

func (svc *Service) handleListMessages(ctx context.Context, _ *mcp.CallToolRequest, params *ListMessagesParams) (*mcp.CallToolResult, any, error) {
	if err := requireFields(map[string]string{"repo": params.Repo}); err != nil {
		return nil, nil, err
	}
	msgs, err := svc.Store.ListMessages(ctx, params.Repo, store.ListOptions{
		IncludeRead: params.IncludeRead,
		Limit:       params.Limit,
		SinceID:     params.SinceID,
		OmitBody:    params.OmitBody,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list_messages: %w", err)
	}

	views := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		views = append(views, toMessageView(m))
	}

	text := fmt.Sprintf("%d message(s) for %s (include_read=%t)", len(views), params.Repo, params.IncludeRead)
	return textResult(text), ListMessagesResult{Messages: views}, nil
}

// MarkReadParams identifies the message to mark.
type MarkReadParams struct {
	ID int64 `json:"id" jsonschema:"the message id returned by list_messages"`
}

// MarkReadResult reports whether the row was updated by this call.
type MarkReadResult struct {
	Updated bool `json:"updated"`
}

func (svc *Service) handleMarkRead(ctx context.Context, _ *mcp.CallToolRequest, params *MarkReadParams) (*mcp.CallToolResult, any, error) {
	if params.ID <= 0 {
		return nil, nil, fmt.Errorf("missing required field: id")
	}
	updated, err := svc.Store.MarkRead(ctx, params.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("mark_read: %w", err)
	}
	text := fmt.Sprintf("mark_read id=%d updated=%t", params.ID, updated)
	return textResult(text), MarkReadResult{Updated: updated}, nil
}

// MarkReadBulkParams identifies the messages to mark in a single call.
type MarkReadBulkParams struct {
	IDs []int64 `json:"ids" jsonschema:"message ids to mark as read"`
}

// MarkReadBulkResult reports how many rows were newly updated.
type MarkReadBulkResult struct {
	Updated int64 `json:"updated"`
}

func (svc *Service) handleMarkReadBulk(ctx context.Context, _ *mcp.CallToolRequest, params *MarkReadBulkParams) (*mcp.CallToolResult, any, error) {
	if len(params.IDs) == 0 {
		return nil, nil, fmt.Errorf("missing required field: ids")
	}
	if len(params.IDs) > store.MaxListLimit {
		return nil, nil, fmt.Errorf("too many ids: %d (max %d)", len(params.IDs), store.MaxListLimit)
	}
	for _, id := range params.IDs {
		if id <= 0 {
			return nil, nil, fmt.Errorf("invalid id in ids: %d (must be > 0)", id)
		}
	}
	updated, err := svc.Store.MarkReadBulk(ctx, params.IDs)
	if err != nil {
		return nil, nil, fmt.Errorf("mark_read_bulk: %w", err)
	}
	text := fmt.Sprintf("mark_read_bulk count=%d updated=%d", len(params.IDs), updated)
	return textResult(text), MarkReadBulkResult{Updated: updated}, nil
}

// ListSessionsParams takes no arguments but is defined so the SDK can derive
// an empty JSON schema.
type ListSessionsParams struct{}

// ListSessionsResult is the snapshot of currently registered sessions.
type ListSessionsResult struct {
	Sessions []SessionView `json:"sessions"`
}

func (svc *Service) handleListSessions(ctx context.Context, _ *mcp.CallToolRequest, _ *ListSessionsParams) (*mcp.CallToolResult, any, error) {
	sessions, err := svc.Store.ListSessions(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list_sessions: %w", err)
	}
	views := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, toSessionView(s))
	}
	text := fmt.Sprintf("%d registered session(s)", len(views))
	return textResult(text), ListSessionsResult{Sessions: views}, nil
}

// requireFields returns a descriptive error naming the first empty field, or
// nil when every value is non-empty. The iteration order is fixed so tests
// asserting exact error text don't flap across runs.
func requireFields(fields map[string]string) error {
	for _, name := range []string{"repo", "pane", "from", "to", "subject"} {
		v, ok := fields[name]
		if !ok {
			continue
		}
		if v == "" {
			return fmt.Errorf("missing required field: %s", name)
		}
	}
	return nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
