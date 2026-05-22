package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stephenrhoton/micro-status-mcp/internal/store"
	"github.com/stephenrhoton/micro-status-mcp/internal/tmuxnotify"
)

// recordingNotifier captures every Notify call so tests can assert on them.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
	err   error
}

type notifyCall struct {
	Pane    string
	Sender  string
	Subject string
}

func (r *recordingNotifier) Notify(_ context.Context, pane, sender, subject string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, notifyCall{Pane: pane, Sender: sender, Subject: subject})
	return r.err
}

func (r *recordingNotifier) Calls() []notifyCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notifyCall(nil), r.calls...)
}

// newTestStore opens an in-memory store with a deterministic clock.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// envFixture bundles the moving parts of an HTTP-mounted MCP server test
// so callers don't deal with returning three values.
type envFixture struct {
	Server   *httptest.Server
	Notifier *recordingNotifier
	Store    *store.Store
	Session  *mcp.ClientSession
}

// newTestEnv spins up an in-memory store, the given notifier (or a recording
// one if nil), an httptest.Server wrapping HTTPHandler, and a connected client
// session.
func newTestEnv(t *testing.T, notif Notifier) *envFixture {
	t.Helper()
	st := newTestStore(t)
	rec, _ := notif.(*recordingNotifier)
	if notif == nil {
		rec = &recordingNotifier{}
		notif = rec
	}
	svc := &Service{Store: st, Notifier: notif}
	handler := HTTPHandler(svc, &mcp.Implementation{Name: "test", Version: "test"})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	sess := dialSession(t, ts.URL)
	return &envFixture{Server: ts, Notifier: rec, Store: st, Session: sess}
}

func dialSession(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// callJSON invokes a tool and unmarshals the structured content into out.
func callJSON(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool %s returned error: %+v", name, res.Content)
	}
	if out != nil {
		if res.StructuredContent == nil {
			t.Fatalf("CallTool %s: no structured content; content=%+v", name, res.Content)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal structured content: %v", err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("unmarshal into %T: %v (raw=%s)", out, err, raw)
		}
	}
	return res
}

// callExpectError invokes a tool and asserts that it returns IsError=true
// with content matching wantSubstr.
func callExpectError(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any, wantSubstr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("CallTool %s: expected IsError=true, got success", name)
	}
	found := false
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, wantSubstr) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("CallTool %s: expected error containing %q, got %+v", name, wantSubstr, res.Content)
	}
}

func TestEndToEnd_PostListMarkRead(t *testing.T) {
	env := newTestEnv(t, nil)

	var regRes RegisterResult
	callJSON(t, env.Session, "register", map[string]any{"repo": "alpha", "pane": "main:0.1"}, &regRes)
	if regRes.Repo != "alpha" || regRes.Pane != "main:0.1" {
		t.Errorf("register alpha: unexpected result %+v", regRes)
	}
	callJSON(t, env.Session, "register", map[string]any{"repo": "beta", "pane": "main:0.2"}, &regRes)

	var post PostMessageResult
	callJSON(t, env.Session, "post_message", map[string]any{
		"from": "alpha", "to": "beta", "subject": "hello", "body": "world",
	}, &post)
	if post.ID == 0 {
		t.Error("expected non-zero message id")
	}
	if !post.Notified {
		t.Errorf("expected notification (beta is registered); got %+v", post)
	}

	calls := env.Notifier.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 notify call, got %d", len(calls))
	}
	want := notifyCall{Pane: "main:0.2", Sender: "alpha", Subject: "hello"}
	if calls[0] != want {
		t.Errorf("notify call: got %+v, want %+v", calls[0], want)
	}

	var listed ListMessagesResult
	callJSON(t, env.Session, "list_messages", map[string]any{"repo": "beta"}, &listed)
	if len(listed.Messages) != 1 {
		t.Fatalf("expected 1 unread message for beta, got %d", len(listed.Messages))
	}
	if listed.Messages[0].Subject != "hello" {
		t.Errorf("unexpected subject: %q", listed.Messages[0].Subject)
	}
	if listed.Messages[0].ID != post.ID {
		t.Errorf("expected id %d, got %d", post.ID, listed.Messages[0].ID)
	}

	var mark MarkReadResult
	callJSON(t, env.Session, "mark_read", map[string]any{"id": post.ID}, &mark)
	if !mark.Updated {
		t.Error("expected mark_read to report Updated=true")
	}

	callJSON(t, env.Session, "list_messages", map[string]any{"repo": "beta"}, &listed)
	if len(listed.Messages) != 0 {
		t.Errorf("expected empty list after mark_read, got %d", len(listed.Messages))
	}

	callJSON(t, env.Session, "list_messages", map[string]any{"repo": "beta", "include_read": true}, &listed)
	if len(listed.Messages) != 1 {
		t.Fatalf("expected 1 message with include_read, got %d", len(listed.Messages))
	}
	if listed.Messages[0].ReadAt == nil {
		t.Error("expected ReadAt to be populated")
	}
}

func TestPostMessage_NoSession_NotNotified(t *testing.T) {
	env := newTestEnv(t, nil)

	var post PostMessageResult
	callJSON(t, env.Session, "post_message", map[string]any{
		"from": "alpha", "to": "ghost", "subject": "?", "body": "anyone?",
	}, &post)
	if post.Notified {
		t.Errorf("expected Notified=false when recipient not registered, got %+v", post)
	}
	if len(env.Notifier.Calls()) != 0 {
		t.Errorf("expected no notifier calls, got %d", len(env.Notifier.Calls()))
	}
	if post.ID == 0 {
		t.Error("message should still be persisted")
	}
}

func TestPaneMissing_PrunesRegistration(t *testing.T) {
	env := newTestEnv(t, paneMissingNotifier{})

	var regRes RegisterResult
	callJSON(t, env.Session, "register", map[string]any{"repo": "beta", "pane": "main:0.99"}, &regRes)

	var post PostMessageResult
	callJSON(t, env.Session, "post_message", map[string]any{
		"from": "alpha", "to": "beta", "subject": "ping", "body": "are you there?",
	}, &post)
	if post.Notified {
		t.Error("expected Notified=false when pane is missing")
	}

	var sessions ListSessionsResult
	callJSON(t, env.Session, "list_sessions", map[string]any{}, &sessions)
	for _, s := range sessions.Sessions {
		if s.Repo == "beta" {
			t.Errorf("expected stale beta registration to be pruned, still present: %+v", s)
		}
	}
}

func TestPaneMissing_DoesNotClobberRereg(t *testing.T) {
	// Race: the recipient re-registers with a new pane *between* the
	// server's GetSession lookup and the prune that follows ErrPaneMissing.
	// The prune must scope to the exact pane it tried to notify so the
	// fresh registration survives. Simulated by a notifier that re-registers
	// inside Notify() before returning ErrPaneMissing.
	st := newTestStore(t)
	if _, _, err := st.UpsertSession(context.Background(), "beta", "main:0.99"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rereg := &rereggingNotifier{store: st, repo: "beta", newPane: "main:0.42"}
	svc := &Service{Store: st, Notifier: rereg}
	handler := HTTPHandler(svc, &mcp.Implementation{Name: "rereg", Version: "test"})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	sess := dialSession(t, ts.URL)

	var post PostMessageResult
	callJSON(t, sess, "post_message", map[string]any{
		"from": "alpha", "to": "beta", "subject": "x", "body": "y",
	}, &post)

	var sessions ListSessionsResult
	callJSON(t, sess, "list_sessions", map[string]any{}, &sessions)
	found := ""
	for _, s := range sessions.Sessions {
		if s.Repo == "beta" {
			found = s.Pane
		}
	}
	if found != "main:0.42" {
		t.Errorf("expected fresh registration (main:0.42) to survive, got %q", found)
	}
}

// rereggingNotifier re-registers the recipient with a new pane between the
// time the server reads its registration and the time the prune runs.
type rereggingNotifier struct {
	store   *store.Store
	repo    string
	newPane string
}

func (r *rereggingNotifier) Notify(ctx context.Context, _, _, _ string) error {
	// The handler has already read the old pane; simulate the recipient
	// re-registering before we report the failure.
	_, _, _ = r.store.UpsertSession(ctx, r.repo, r.newPane)
	return tmuxnotify.ErrPaneMissing
}

func TestTmuxUnavailable_KeepsRegistration(t *testing.T) {
	env := newTestEnv(t, tmuxUnavailableNotifier{})

	var regRes RegisterResult
	callJSON(t, env.Session, "register", map[string]any{"repo": "beta", "pane": "main:0.2"}, &regRes)

	var post PostMessageResult
	callJSON(t, env.Session, "post_message", map[string]any{
		"from": "alpha", "to": "beta", "subject": "ping", "body": "alive?",
	}, &post)
	if post.Notified {
		t.Error("expected Notified=false when tmux unavailable")
	}
	if post.NotifyError == "" {
		t.Error("expected NotifyError to surface the unavailability")
	}

	var sessions ListSessionsResult
	callJSON(t, env.Session, "list_sessions", map[string]any{}, &sessions)
	found := false
	for _, s := range sessions.Sessions {
		if s.Repo == "beta" {
			found = true
		}
	}
	if !found {
		t.Error("expected beta registration to remain when tmux is unavailable (transient error)")
	}
}

func TestUnregister(t *testing.T) {
	env := newTestEnv(t, nil)

	var reg RegisterResult
	callJSON(t, env.Session, "register", map[string]any{"repo": "alpha", "pane": "main:0.1"}, &reg)

	var un UnregisterResult
	callJSON(t, env.Session, "unregister", map[string]any{"repo": "alpha"}, &un)
	if !un.Removed {
		t.Error("expected Removed=true on first unregister")
	}
	callJSON(t, env.Session, "unregister", map[string]any{"repo": "alpha"}, &un)
	if un.Removed {
		t.Error("expected Removed=false on idempotent second unregister")
	}
}

func TestRequiredFieldValidation(t *testing.T) {
	env := newTestEnv(t, nil)

	callExpectError(t, env.Session, "register",
		map[string]any{"repo": "", "pane": "main:0.1"}, "missing required field: repo")
	callExpectError(t, env.Session, "register",
		map[string]any{"repo": "alpha", "pane": ""}, "missing required field: pane")
	callExpectError(t, env.Session, "post_message",
		map[string]any{"from": "", "to": "beta", "subject": "s", "body": "b"},
		"missing required field: from")
	callExpectError(t, env.Session, "list_messages",
		map[string]any{"repo": ""}, "missing required field: repo")
	callExpectError(t, env.Session, "mark_read",
		map[string]any{"id": 0}, "missing required field: id")
}

func TestPostMessage_BodyTooLarge(t *testing.T) {
	env := newTestEnv(t, nil)
	big := strings.Repeat("a", MaxBodyBytes+1)
	callExpectError(t, env.Session, "post_message", map[string]any{
		"from": "alpha", "to": "beta", "subject": "huge", "body": big,
	}, "body too large")
}

func TestListMessages_OmitBody(t *testing.T) {
	env := newTestEnv(t, nil)
	var post PostMessageResult
	callJSON(t, env.Session, "post_message", map[string]any{
		"from": "alpha", "to": "beta", "subject": "subj", "body": "secret body",
	}, &post)

	var listed ListMessagesResult
	callJSON(t, env.Session, "list_messages",
		map[string]any{"repo": "beta", "omit_body": true}, &listed)
	if len(listed.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(listed.Messages))
	}
	if listed.Messages[0].Body != "" {
		t.Errorf("expected empty body when omit_body=true, got %q", listed.Messages[0].Body)
	}
	if listed.Messages[0].Subject != "subj" {
		t.Errorf("expected subject preserved, got %q", listed.Messages[0].Subject)
	}
}

func TestMarkReadBulk(t *testing.T) {
	env := newTestEnv(t, nil)

	var ids []int64
	for i := 0; i < 3; i++ {
		var p PostMessageResult
		callJSON(t, env.Session, "post_message", map[string]any{
			"from": "alpha", "to": "beta", "subject": "s", "body": "b",
		}, &p)
		ids = append(ids, p.ID)
	}

	var res MarkReadBulkResult
	callJSON(t, env.Session, "mark_read_bulk",
		map[string]any{"ids": ids}, &res)
	if res.Updated != 3 {
		t.Errorf("expected 3 updated, got %d", res.Updated)
	}

	// Validation: empty ids rejected.
	callExpectError(t, env.Session, "mark_read_bulk",
		map[string]any{"ids": []int64{}}, "missing required field: ids")

	// Invalid id rejected.
	callExpectError(t, env.Session, "mark_read_bulk",
		map[string]any{"ids": []int64{0}}, "invalid id in ids")
}

func TestListMessages_Pagination(t *testing.T) {
	env := newTestEnv(t, nil)

	for i := 0; i < 5; i++ {
		var post PostMessageResult
		callJSON(t, env.Session, "post_message", map[string]any{
			"from": "alpha", "to": "beta", "subject": "s", "body": "b",
		}, &post)
	}

	var page ListMessagesResult
	callJSON(t, env.Session, "list_messages",
		map[string]any{"repo": "beta", "limit": 2}, &page)
	if len(page.Messages) != 2 {
		t.Fatalf("expected 2 messages page 1, got %d", len(page.Messages))
	}

	var next ListMessagesResult
	callJSON(t, env.Session, "list_messages", map[string]any{
		"repo": "beta", "limit": 2, "since_id": page.Messages[1].ID,
	}, &next)
	if len(next.Messages) != 2 {
		t.Fatalf("expected 2 messages page 2, got %d", len(next.Messages))
	}
	if next.Messages[0].ID <= page.Messages[1].ID {
		t.Errorf("expected page 2 to start after id %d, got %d", page.Messages[1].ID, next.Messages[0].ID)
	}
}

// paneMissingNotifier always returns ErrPaneMissing so handlers prune state.
type paneMissingNotifier struct{}

func (paneMissingNotifier) Notify(_ context.Context, _, _, _ string) error {
	return tmuxnotify.ErrPaneMissing
}

// tmuxUnavailableNotifier always returns ErrTmuxUnavailable so handlers keep
// state but surface a notify_error.
type tmuxUnavailableNotifier struct{}

func (tmuxUnavailableNotifier) Notify(_ context.Context, _, _, _ string) error {
	return tmuxnotify.ErrTmuxUnavailable
}
