package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/stephenrhoton/micro-status-mcp/internal/server"
	"github.com/stephenrhoton/micro-status-mcp/internal/store"
	"github.com/stephenrhoton/micro-status-mcp/internal/tmuxnotify"
)

const (
	serveReadHeaderTimeout = 5 * time.Second
	serveIdleTimeout       = 120 * time.Second
	serveShutdownTimeout   = 5 * time.Second
	// stateDirPerm protects message bodies — they may contain
	// repo-private information from any Claude session.
	stateDirPerm os.FileMode = 0o700
	// maxRequestBytes caps the HTTP request body size before the MCP SDK
	// reads it into memory. ~4× server.MaxBodyBytes leaves headroom for
	// JSON-RPC envelope overhead.
	maxRequestBytes int64 = 4 << 20
)

func newServeCommand() *cobra.Command {
	var (
		addr   string
		dbPath string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server on a local HTTP endpoint.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), addr, dbPath)
		},
	}

	defaultDB, err := defaultDBPath()
	if err != nil {
		defaultDB = "state.db"
	}

	cmd.Flags().StringVar(&addr, "addr", defaultAddr, "TCP address to listen on")
	cmd.Flags().StringVar(&dbPath, "db", defaultDB, "SQLite database file path")
	return cmd
}

func defaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "micro-status-mcp", "state.db"), nil
}

func runServe(ctx context.Context, addr, dbPath string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(filepath.Dir(dbPath), stateDirPerm); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", dbPath, err)
	}
	defer func() { _ = st.Close() }()

	svc := &server.Service{
		Store:    st,
		Notifier: tmuxnotify.New(),
		Logger:   logger,
	}

	mcpHandler := server.HTTPHandler(svc, &mcp.Implementation{
		Name:    "micro-status-mcp",
		Version: Version,
	})

	mux := http.NewServeMux()
	// Cap request body before the SDK io.ReadAlls it.
	mux.Handle(mcpPath, http.MaxBytesHandler(mcpHandler, maxRequestBytes))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: serveReadHeaderTimeout,
		IdleTimeout:       serveIdleTimeout,
	}

	// Graceful shutdown on Ctrl-C / SIGTERM.
	shutdownCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var (
		wg       sync.WaitGroup
		listenCh = make(chan error, 1)
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("listening", "addr", addr, "mcp_path", mcpPath, "db", dbPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenCh <- err
		}
	}()

	select {
	case <-shutdownCtx.Done():
		logger.Info("shutting down")
		stopCtx, stopCancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		defer stopCancel()
		if err := httpServer.Shutdown(stopCtx); err != nil {
			// Even if Shutdown errored, wait for the listener goroutine
			// to exit before returning so deferred resources (DB handle)
			// are not torn down while requests are in flight.
			wg.Wait()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		wg.Wait()
		return nil
	case err := <-listenCh:
		// ListenAndServe failed; wait for the goroutine to fully exit.
		wg.Wait()
		return fmt.Errorf("listen: %w", err)
	}
}
