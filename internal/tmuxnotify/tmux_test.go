package tmuxnotify

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls   []call
	results []runResult
}

type runResult struct {
	out []byte
	err error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	if len(f.calls) > len(f.results) {
		return nil, nil
	}
	r := f.results[len(f.calls)-1]
	return r.out, r.err
}

func TestNotify_Success(t *testing.T) {
	r := &fakeRunner{
		results: []runResult{{}, {}}, // both send-keys calls succeed
	}
	// Tiny settle so the test does not wait the full default delay.
	n := &Notifier{Runner: r, SettleDelay: time.Microsecond}
	if err := n.Notify(context.Background(), "main:0.1", "alpha", "hello"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if len(r.calls) != 2 {
		t.Fatalf("expected 2 tmux calls (type then submit), got %d", len(r.calls))
	}
	wantType := call{name: "tmux", args: []string{"send-keys", "-t", "main:0.1", "/inbox  # new from alpha: hello"}}
	if !reflect.DeepEqual(r.calls[0], wantType) {
		t.Errorf("type call: got %+v, want %+v", r.calls[0], wantType)
	}
	wantEnter := call{name: "tmux", args: []string{"send-keys", "-t", "main:0.1", "Enter"}}
	if !reflect.DeepEqual(r.calls[1], wantEnter) {
		t.Errorf("submit call: got %+v, want %+v", r.calls[1], wantEnter)
	}
}

func TestNotify_NoEnterWhenTypeFails(t *testing.T) {
	// If typing the message fails (pane gone), we must NOT send a stray Enter.
	r := &fakeRunner{
		results: []runResult{
			{out: []byte("can't find pane: main:0.99"), err: errors.New("exit status 1")},
		},
	}
	n := &Notifier{Runner: r, SettleDelay: time.Microsecond}
	if err := n.Notify(context.Background(), "main:0.99", "alpha", "hello"); !errors.Is(err, ErrPaneMissing) {
		t.Fatalf("expected ErrPaneMissing, got %v", err)
	}
	if len(r.calls) != 1 {
		t.Errorf("expected exactly 1 tmux call (type only, no Enter), got %d", len(r.calls))
	}
}

func TestNotify_PaneMissing(t *testing.T) {
	cases := map[string]string{
		"can't find pane":    "can't find pane: main:0.99",
		"can't find session": "can't find session: main",
		"session not found":  "session not found: main",
		"no such pane":       "no such pane",
	}
	for name, stderr := range cases {
		t.Run(name, func(t *testing.T) {
			r := &fakeRunner{
				results: []runResult{
					{out: []byte(stderr), err: errors.New("exit status 1")},
				},
			}
			n := &Notifier{Runner: r}
			err := n.Notify(context.Background(), "main:0.99", "alpha", "hello")
			if !errors.Is(err, ErrPaneMissing) {
				t.Fatalf("expected ErrPaneMissing, got %v", err)
			}
			if len(r.calls) != 1 {
				t.Errorf("expected exactly 1 tmux call, got %d", len(r.calls))
			}
		})
	}
}

func TestNotify_TmuxUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		err    error
	}{
		{name: "no server", stderr: "no server running on /tmp/tmux-501/default", err: errors.New("exit status 1")},
		{name: "connection error", stderr: "error connecting to /tmp/tmux-501/default", err: errors.New("exit status 1")},
		{name: "binary missing", stderr: "", err: exec.ErrNotFound},
		{name: "unrecognized", stderr: "something nobody has seen", err: errors.New("exit status 1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{
				results: []runResult{
					{out: []byte(tc.stderr), err: tc.err},
				},
			}
			n := &Notifier{Runner: r}
			err := n.Notify(context.Background(), "main:0.1", "alpha", "hello")
			if !errors.Is(err, ErrTmuxUnavailable) {
				t.Errorf("%s: expected ErrTmuxUnavailable, got %v", tc.name, err)
			}
			if errors.Is(err, ErrPaneMissing) {
				t.Errorf("%s: expected NOT to be ErrPaneMissing, but it was", tc.name)
			}
		})
	}
}

func TestNotify_RejectsEmptyPane(t *testing.T) {
	r := &fakeRunner{}
	n := &Notifier{Runner: r}
	if err := n.Notify(context.Background(), "", "alpha", "hello"); err == nil {
		t.Error("expected error for empty pane")
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no tmux calls, got %d", len(r.calls))
	}
}

func TestSanitizeSubject(t *testing.T) {
	cases := map[string]string{
		"hello":          "hello",
		"line1\nline2":   "line1 line2",
		"line1\r\nline2": "line1  line2",
		"tab\there":      "tab here",
		"bell\x07word":   "bellword",
		"":               "",
	}
	for in, want := range cases {
		if got := sanitizeSubject(in); got != want {
			t.Errorf("sanitizeSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeSubject_TruncatesLong(t *testing.T) {
	long := strings.Repeat("a", maxSubjectLen+50)
	got := sanitizeSubject(long)
	if !strings.HasPrefix(got, strings.Repeat("a", maxSubjectLen)) {
		t.Errorf("expected truncated prefix of %d 'a' chars", maxSubjectLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got[len(got)-5:])
	}
}

func TestSanitizeSubject_PreservesRuneBoundaries(t *testing.T) {
	// 198 ASCII bytes + a 4-byte rune (🦊) would put the 4-byte rune
	// straddling the maxSubjectLen=200 boundary. Truncation must happen
	// on a rune boundary so the result is valid UTF-8.
	prefix := strings.Repeat("a", maxSubjectLen-2)
	in := prefix + "🦊"
	got := sanitizeSubject(in)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncation with ellipsis, got %q", got)
	}
	// The body before "…" must be valid UTF-8 (no truncation mid-rune).
	body := strings.TrimSuffix(got, "…")
	if !utf8.ValidString(body) {
		t.Fatalf("sanitizeSubject produced invalid UTF-8: %q", got)
	}
}
