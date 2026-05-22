package store

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	fixed := time.Date(2026, 5, 22, 14, 30, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return fixed })
	return s
}

func TestUpsertSession_InsertThenUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	prev, saved, err := s.UpsertSession(ctx, "alpha", "main:0.1")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if prev != "" {
		t.Errorf("expected empty previous pane, got %q", prev)
	}
	if saved.Repo != "alpha" || saved.Pane != "main:0.1" {
		t.Errorf("unexpected saved session: %+v", saved)
	}

	prev, _, err = s.UpsertSession(ctx, "alpha", "main:0.2")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if prev != "main:0.1" {
		t.Errorf("expected previous pane main:0.1, got %q", prev)
	}

	got, err := s.GetSession(ctx, "alpha")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Pane != "main:0.2" {
		t.Errorf("expected pane main:0.2, got %q", got.Pane)
	}
}

func TestUpsertSession_RejectsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, _, err := s.UpsertSession(ctx, "", "main:0.1"); err == nil {
		t.Error("expected error for empty repo")
	}
	if _, _, err := s.UpsertSession(ctx, "alpha", ""); err == nil {
		t.Error("expected error for empty pane")
	}
}

func TestDeleteSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, _, err := s.UpsertSession(ctx, "alpha", "main:0.1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	deleted, err := s.DeleteSession(ctx, "alpha")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted {
		t.Error("expected delete to report a removed row")
	}

	deleted, err = s.DeleteSession(ctx, "alpha")
	if err != nil {
		t.Fatalf("delete (second): %v", err)
	}
	if deleted {
		t.Error("expected idempotent delete to report no row removed")
	}

	if _, err := s.GetSession(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListSessions_OrderedAndNonNil(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	empty, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list (empty): %v", err)
	}
	if empty == nil {
		t.Error("expected non-nil empty slice for empty result")
	}
	if len(empty) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(empty))
	}

	for _, repo := range []string{"gamma", "alpha", "beta"} {
		if _, _, err := s.UpsertSession(ctx, repo, repo+":0.0"); err != nil {
			t.Fatalf("upsert %s: %v", repo, err)
		}
	}
	got, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("expected %d sessions, got %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i].Repo != w {
			t.Errorf("position %d: expected %q, got %q", i, w, got[i].Repo)
		}
	}
}

func TestPostAndListMessages_Order(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	t1 := time.Date(2026, 5, 22, 14, 30, 0, 0, time.UTC)
	t2 := t1.Add(1 * time.Second)
	t3 := t2.Add(1 * time.Second)
	times := []time.Time{t1, t2, t3}
	idx := 0
	s.SetClock(func() time.Time {
		t := times[idx]
		idx++
		return t
	})

	if _, err := s.PostMessage(ctx, "alpha", "beta", "first", "body1"); err != nil {
		t.Fatalf("post 1: %v", err)
	}
	if _, err := s.PostMessage(ctx, "gamma", "alpha", "other", "body2"); err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if _, err := s.PostMessage(ctx, "alpha", "beta", "second", "body3"); err != nil {
		t.Fatalf("post 3: %v", err)
	}

	betaMsgs, err := s.ListMessages(ctx, "beta", ListOptions{})
	if err != nil {
		t.Fatalf("list beta: %v", err)
	}
	if len(betaMsgs) != 2 {
		t.Fatalf("expected 2 messages for beta, got %d", len(betaMsgs))
	}
	if betaMsgs[0].Subject != "first" || betaMsgs[1].Subject != "second" {
		t.Errorf("unexpected order: %q, %q", betaMsgs[0].Subject, betaMsgs[1].Subject)
	}
}

func TestListMessages_Pagination(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	base := time.Date(2026, 5, 22, 14, 30, 0, 0, time.UTC)
	i := 0
	s.SetClock(func() time.Time {
		t := base.Add(time.Duration(i) * time.Second)
		i++
		return t
	})

	for n := 0; n < 5; n++ {
		if _, err := s.PostMessage(ctx, "alpha", "beta", "subj", "body"); err != nil {
			t.Fatalf("post %d: %v", n, err)
		}
	}

	// Limit clamps result.
	page, err := s.ListMessages(ctx, "beta", ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(page))
	}

	// SinceID skips earlier IDs.
	next, err := s.ListMessages(ctx, "beta", ListOptions{Limit: 2, SinceID: page[1].ID})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(next) != 2 {
		t.Fatalf("expected 2 messages page 2, got %d", len(next))
	}
	if next[0].ID != page[1].ID+1 {
		t.Errorf("expected next page to start at ID %d, got %d", page[1].ID+1, next[0].ID)
	}
}

func TestListMessages_LimitClampedToMax(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	got, err := s.ListMessages(ctx, "ghost", ListOptions{Limit: 999_999})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Error("expected non-nil empty slice")
	}
	if cap(got) > MaxListLimit {
		t.Errorf("expected capacity <= %d, got %d", MaxListLimit, cap(got))
	}
}

func TestPostMessage_RejectsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.PostMessage(ctx, "", "beta", "subj", "b"); err == nil {
		t.Error("expected error for empty from")
	}
	if _, err := s.PostMessage(ctx, "alpha", "", "subj", "b"); err == nil {
		t.Error("expected error for empty to")
	}
	if _, err := s.PostMessage(ctx, "alpha", "beta", "", "b"); err == nil {
		t.Error("expected error for empty subject")
	}
}

func TestMarkRead_HidesFromDefaultList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	m, err := s.PostMessage(ctx, "alpha", "beta", "subj", "body")
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	updated, err := s.MarkRead(ctx, m.ID)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if !updated {
		t.Error("expected mark_read to update a row")
	}

	updated, err = s.MarkRead(ctx, m.ID)
	if err != nil {
		t.Fatalf("mark read (again): %v", err)
	}
	if updated {
		t.Error("expected second mark_read to be a no-op")
	}

	unread, err := s.ListMessages(ctx, "beta", ListOptions{})
	if err != nil {
		t.Fatalf("list unread: %v", err)
	}
	if len(unread) != 0 {
		t.Errorf("expected no unread messages, got %d", len(unread))
	}

	all, err := s.ListMessages(ctx, "beta", ListOptions{IncludeRead: true})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 message in include_read list, got %d", len(all))
	}
	if all[0].ReadAt == nil {
		t.Error("expected ReadAt to be populated after MarkRead")
	}
}

func TestMarkRead_UnknownID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	updated, err := s.MarkRead(ctx, 9999)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if updated {
		t.Error("expected no-op for unknown id")
	}
}

func TestListMessages_OmitBody(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.PostMessage(ctx, "alpha", "beta", "subj", "long body here"); err != nil {
		t.Fatalf("post: %v", err)
	}
	got, err := s.ListMessages(ctx, "beta", ListOptions{OmitBody: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Body != "" {
		t.Errorf("expected empty body, got %q", got[0].Body)
	}
	if got[0].Subject != "subj" {
		t.Errorf("expected subject preserved, got %q", got[0].Subject)
	}
}

func TestMarkReadBulk(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	ids := []int64{}
	for i := 0; i < 3; i++ {
		m, err := s.PostMessage(ctx, "alpha", "beta", "s", "b")
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		ids = append(ids, m.ID)
	}

	n, err := s.MarkReadBulk(ctx, ids)
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 updated, got %d", n)
	}

	// Idempotent: re-marking returns 0.
	n, err = s.MarkReadBulk(ctx, ids)
	if err != nil {
		t.Fatalf("bulk replay: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 updated on replay, got %d", n)
	}

	// Empty slice is a no-op.
	n, err = s.MarkReadBulk(ctx, nil)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 for empty input, got %d", n)
	}
}

func TestMergeDefaultPragmas(t *testing.T) {
	t.Run("appends to bare path", func(t *testing.T) {
		got, err := mergeDefaultPragmas("state.db")
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		mustContainPragmas(t, got)
	})

	t.Run("preserves caller pragmas", func(t *testing.T) {
		got, err := mergeDefaultPragmas("state.db?_pragma=journal_mode(MEMORY)")
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if !strings.Contains(got, "journal_mode%28MEMORY%29") &&
			!strings.Contains(got, "journal_mode(MEMORY)") {
			t.Errorf("caller's journal_mode override lost: %q", got)
		}
		if strings.Contains(got, "journal_mode%28WAL%29") ||
			strings.Contains(got, "journal_mode(WAL)") {
			t.Errorf("default journal_mode should not have overridden caller's: %q", got)
		}
		// busy_timeout should still be appended.
		mustContain(t, got, "busy_timeout")
	})

	t.Run("merges with unrelated existing query", func(t *testing.T) {
		got, err := mergeDefaultPragmas("state.db?cache=shared")
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		mustContain(t, got, "cache=shared")
		mustContainPragmas(t, got)
	})

	t.Run("invalid query returns error", func(t *testing.T) {
		if _, err := mergeDefaultPragmas("state.db?%zz"); err == nil {
			t.Error("expected error for invalid query")
		}
	})
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected %q to contain %q", haystack, needle)
	}
}

func mustContainPragmas(t *testing.T, dsn string) {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %q: %v", dsn, err)
	}
	pragmas := parsed.Query()["_pragma"]
	want := map[string]bool{
		"journal_mode": false,
		"busy_timeout": false,
		"synchronous":  false,
		"foreign_keys": false,
	}
	for _, p := range pragmas {
		for name := range want {
			if strings.HasPrefix(p, name+"(") {
				want[name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("default pragma %s missing from %q (got pragmas: %v)", name, dsn, pragmas)
		}
	}
}

func TestPoolSizeFor(t *testing.T) {
	cases := map[string]int{
		":memory:":                       1,
		"":                               1,
		"file::memory:":                  1,
		"state.db?mode=memory":           1,
		"file:state.db?mode=memory":      1,
		"state.db":                       readerPoolSize,
		"state.db?cache=shared":          readerPoolSize,
		"/Users/me/.claude/foo/state.db": readerPoolSize,
	}
	for dsn, want := range cases {
		if got := poolSizeFor(dsn); got != want {
			t.Errorf("poolSizeFor(%q) = %d, want %d", dsn, got, want)
		}
	}
}

func TestDeleteSessionByPane(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, _, err := s.UpsertSession(ctx, "alpha", "main:0.1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Wrong pane -> no delete.
	removed, err := s.DeleteSessionByPane(ctx, "alpha", "main:0.99")
	if err != nil {
		t.Fatalf("by-pane (wrong): %v", err)
	}
	if removed {
		t.Error("expected no delete when pane mismatches")
	}
	if _, err := s.GetSession(ctx, "alpha"); err != nil {
		t.Errorf("session should still exist: %v", err)
	}

	// Right pane -> deletes.
	removed, err = s.DeleteSessionByPane(ctx, "alpha", "main:0.1")
	if err != nil {
		t.Fatalf("by-pane (right): %v", err)
	}
	if !removed {
		t.Error("expected delete when pane matches")
	}
	if _, err := s.GetSession(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestDurability_AcrossOpens(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")

	{
		s, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("first open: %v", err)
		}
		if _, _, err := s.UpsertSession(ctx, "alpha", "main:0.1"); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := s.PostMessage(ctx, "alpha", "beta", "still here?", "hi"); err != nil {
			t.Fatalf("post: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sess, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sess) != 1 || sess[0].Repo != "alpha" {
		t.Errorf("expected alpha session to survive, got %+v", sess)
	}

	msgs, err := s.ListMessages(ctx, "beta", ListOptions{})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "still here?" {
		t.Errorf("expected message to survive, got %+v", msgs)
	}
}
