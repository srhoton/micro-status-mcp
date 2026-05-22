package cli

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// freePort returns an OS-assigned free TCP port.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestRunServe_CleanShutdown spins up runServe on an ephemeral port + temp DB
// and verifies that:
//   - /healthz responds OK before shutdown
//   - canceling the parent context returns runServe cleanly with no error
//   - the listener goroutine is fully drained before runServe returns
func TestRunServe_CleanShutdown(t *testing.T) {
	addr := freePort(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(ctx, addr, dbPath)
	}()

	// Wait for the server to be ready (poll /healthz).
	deadline := time.Now().Add(5 * time.Second)
	url := "http://" + addr + "/healthz"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start in time (last err=%v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Trigger graceful shutdown.
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runServe returned error on graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServe did not return within shutdown deadline")
	}
}

// TestRunServe_ListenFailure verifies that a bind failure surfaces as an
// error rather than hanging.
func TestRunServe_ListenFailure(t *testing.T) {
	// Bind a placeholder first so runServe's bind will fail.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	defer func() { _ = l.Close() }()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = runServe(ctx, addr, dbPath)
	if err == nil {
		t.Fatal("expected listen error on conflicting bind")
	}
}
