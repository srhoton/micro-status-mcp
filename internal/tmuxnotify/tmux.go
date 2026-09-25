// Package tmuxnotify wakes up a recipient Claude session by send-keying the
// "/inbox" slash command into its tmux pane.
package tmuxnotify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultExecTimeout caps each tmux subprocess call so a wedged tmux can
// never block the request path indefinitely.
const DefaultExecTimeout = 2 * time.Second

// DefaultSettleDelay is the pause between send-keying the message text and
// send-keying Enter. It gives the recipient's input box time to finish
// ingesting the pasted text before the submit keystroke arrives, so the
// Enter registers against the complete message rather than racing it.
const DefaultSettleDelay = 100 * time.Millisecond

// maxSubjectLen bounds the length of the subject we send-key into the
// recipient pane. Anything longer is truncated.
const maxSubjectLen = 200

// Runner abstracts process execution so tests can substitute a fake.
type Runner interface {
	// Run executes name with args. It returns the combined output and any
	// error from the process (non-zero exit status counts as an error).
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands via os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ErrPaneMissing is returned when the target tmux pane no longer exists
// (e.g., the user closed it). Callers typically prune the corresponding
// session registration on this error.
var ErrPaneMissing = errors.New("tmuxnotify: target pane is missing")

// ErrTmuxUnavailable is returned when tmux itself cannot be reached — the
// binary is missing, no tmux server is running, or $TMUX is unset. Callers
// should NOT prune session state on this error; the pane may still exist
// once tmux is reachable again.
var ErrTmuxUnavailable = errors.New("tmuxnotify: tmux is unavailable")

// Notifier delivers wake-up keystrokes to tmux panes.
type Notifier struct {
	Runner Runner
	// ExecTimeout overrides DefaultExecTimeout. Zero means use the default.
	ExecTimeout time.Duration
	// SettleDelay overrides DefaultSettleDelay, the pause between typing the
	// message and submitting Enter. Zero means use the default.
	SettleDelay time.Duration
}

// New returns a Notifier backed by the real `tmux` binary.
func New() *Notifier {
	return &Notifier{Runner: ExecRunner{}}
}

func (n *Notifier) timeout() time.Duration {
	if n.ExecTimeout > 0 {
		return n.ExecTimeout
	}
	return DefaultExecTimeout
}

func (n *Notifier) settle() time.Duration {
	if n.SettleDelay > 0 {
		return n.SettleDelay
	}
	return DefaultSettleDelay
}

// Notify sends `/inbox  # new from <sender>: <subject>` into the given tmux
// pane (e.g. "main:0.1") and then submits it with Enter. On ErrPaneMissing the
// caller should prune the session registration; on ErrTmuxUnavailable it should
// not (tmux may come back online).
//
// The text and the Enter are sent as two separate send-keys calls with a short
// settle delay in between. Combining them into a single `send-keys <msg> Enter`
// races the recipient input box's paste handling: the Enter can arrive before
// the box finishes ingesting the text, leaving the message typed but never
// submitted. tmux send-keys returns a recognizable non-zero exit when the
// target pane is missing, so we classify failures from the first call by stderr.
func (n *Notifier) Notify(ctx context.Context, pane, sender, subject string) error {
	if pane == "" {
		return fmt.Errorf("tmuxnotify: pane is required")
	}

	sendCtx, cancel := context.WithTimeout(ctx, n.timeout())
	defer cancel()
	msg := fmt.Sprintf("/inbox  # new from %s: %s", sender, sanitizeSubject(subject))

	// Call 1: type the message text (no Enter).
	if out, err := n.Runner.Run(sendCtx, "tmux", "send-keys", "-t", pane, msg); err != nil {
		return classifyTmuxError(out, err)
	}

	// Let the input box settle before submitting so Enter registers against
	// the fully-pasted text. Abort if the send context is already done.
	select {
	case <-sendCtx.Done():
		return fmt.Errorf("%w: %v", ErrTmuxUnavailable, sendCtx.Err())
	case <-time.After(n.settle()):
	}

	// Call 2: submit with Enter as a separate keystroke.
	if out, err := n.Runner.Run(sendCtx, "tmux", "send-keys", "-t", pane, "Enter"); err != nil {
		return classifyTmuxError(out, err)
	}
	return nil
}

// classifyTmuxError inspects the combined output of a failing tmux call to
// distinguish "pane has been closed" (recoverable; prune state) from "tmux is
// not reachable" (transient; keep state).
func classifyTmuxError(out []byte, err error) error {
	trimmed := strings.TrimSpace(string(out))
	lower := strings.ToLower(trimmed)

	// "no tmux" cases:
	//   "no server running on /tmp/tmux-501/default"
	//   "error connecting to /tmp/tmux-501/default"
	// Also: exec.ErrNotFound if the tmux binary isn't on PATH.
	if errors.Is(err, exec.ErrNotFound) ||
		strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "error connecting") {
		return fmt.Errorf("%w: %s", ErrTmuxUnavailable, trimmed)
	}

	// "pane missing" / "can't find session" cases:
	//   "can't find pane: main:0.99"
	//   "can't find session: main"
	//   "session not found: main"
	if strings.Contains(lower, "can't find pane") ||
		strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "session not found") ||
		strings.Contains(lower, "no such pane") {
		return fmt.Errorf("%w: %s", ErrPaneMissing, trimmed)
	}

	// Unknown failure — be conservative and treat as unavailable rather
	// than pruning user state on a tmux quirk we haven't seen before.
	return fmt.Errorf("%w: %v (%s)", ErrTmuxUnavailable, err, trimmed)
}

// sanitizeSubject strips control characters (notably CR/LF) and truncates the
// subject so it cannot inject extra keystrokes via send-keys. Truncation is
// done at rune boundaries so multi-byte UTF-8 codepoints are never split.
func sanitizeSubject(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20:
			return -1
		default:
			return r
		}
	}, s)
	if len(cleaned) <= maxSubjectLen {
		return cleaned
	}

	// Walk runes until we have <= maxSubjectLen bytes, then append the
	// ellipsis. Using utf8.DecodeRuneInString avoids splitting a multi-byte
	// rune at the cutoff.
	limit := 0
	for i := 0; i < len(cleaned); {
		_, size := utf8.DecodeRuneInString(cleaned[i:])
		if i+size > maxSubjectLen {
			break
		}
		i += size
		limit = i
	}
	return cleaned[:limit] + "…"
}
