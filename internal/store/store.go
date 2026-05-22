// Package store persists the agent mailbox state (registered sessions and
// messages) in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DefaultListLimit is the default maximum number of messages returned from a
// single ListMessages call when the caller does not specify a limit.
const DefaultListLimit = 100

// MaxListLimit is the hard upper bound enforced on ListMessages, regardless of
// what the caller requested.
const MaxListLimit = 1000

// Session is a Claude session that has announced its tmux pane.
type Session struct {
	Repo         string
	Pane         string
	RegisteredAt time.Time
}

// Message is a single inter-session message.
type Message struct {
	ID       int64
	From     string
	To       string
	Subject  string
	Body     string
	PostedAt time.Time
	ReadAt   *time.Time
}

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("store: not found")

// defaultPragmas are pragmas merged into every DSN at Open time. WAL plus a
// busy timeout lets concurrent readers proceed during writes; foreign_keys
// enforces schema integrity even though our current schema has no FK. Each
// pragma is merged independently — callers can override any single value by
// supplying their own `_pragma=NAME(...)` in the DSN.
var defaultPragmas = map[string]string{
	"journal_mode": "WAL",
	"busy_timeout": "5000",
	"synchronous":  "NORMAL",
	"foreign_keys": "on",
}

// readerPoolSize is the maximum number of concurrent SQLite connections the
// store opens against a file DB. SQLite serializes writers itself; with WAL
// enabled, readers fan out across these connections. For in-memory DSNs we
// fall back to a pool of 1 because each modernc/sqlite connection to
// `:memory:` is a separate private database (see poolSizeFor).
const readerPoolSize = 4

// Store is the persistence layer. All methods are safe for concurrent use.
// Writes serialize through SQLite's WAL write lock; readers fan out across
// up to readerPoolSize connections for file DBs (or a single connection for
// `:memory:` DSNs, since each in-memory connection is a private DB).
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (and migrates) a SQLite database at dsn. Use ":memory:" for tests
// or a file path for production use. Default pragmas are merged into the DSN
// query string; any pragma the caller has already specified wins.
func Open(ctx context.Context, dsn string) (*Store, error) {
	merged, err := mergeDefaultPragmas(dsn)
	if err != nil {
		return nil, fmt.Errorf("merge pragmas: %w", err)
	}
	db, err := sql.Open("sqlite", merged)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// WAL allows concurrent readers; let a small pool fan out so two
	// Claude sessions never queue against each other on read-only ops.
	// For in-memory DSNs, each connection is a separate database, so
	// we have to pin the pool to 1.
	pool := poolSizeFor(dsn)
	db.SetMaxOpenConns(pool)
	db.SetMaxIdleConns(pool)

	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// poolSizeFor returns the connection pool size appropriate for the given
// DSN. Anything resolving to an in-memory database (`:memory:`, `file::memory:`,
// or DSNs with a `mode=memory` query) gets a pool of 1 because additional
// connections would each be a separate database. Everything else gets
// readerPoolSize.
func poolSizeFor(dsn string) int {
	prefix, query, _ := strings.Cut(dsn, "?")
	if prefix == ":memory:" || prefix == "file::memory:" || prefix == "" {
		return 1
	}
	if vals, err := url.ParseQuery(query); err == nil {
		if vals.Get("mode") == "memory" {
			return 1
		}
	}
	return readerPoolSize
}

// mergeDefaultPragmas parses the DSN's query string and appends any pragma
// from defaultPragmas that the caller hasn't already supplied. The "?"
// separator is added when the DSN has no existing query, so callers can
// pass either `:memory:` or `state.db?cache=shared`.
func mergeDefaultPragmas(dsn string) (string, error) {
	prefix, query, hasQuery := strings.Cut(dsn, "?")

	values, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("parse dsn query: %w", err)
	}

	// Collect pragmas the caller already supplied so we don't override them.
	seen := map[string]bool{}
	for _, v := range values["_pragma"] {
		if name, _, ok := strings.Cut(v, "("); ok {
			seen[strings.TrimSpace(name)] = true
		}
	}
	// Iterate in a stable order so the resulting DSN is deterministic.
	for _, name := range []string{"journal_mode", "busy_timeout", "synchronous", "foreign_keys"} {
		if seen[name] {
			continue
		}
		values.Add("_pragma", fmt.Sprintf("%s(%s)", name, defaultPragmas[name]))
	}

	encoded := values.Encode()
	if encoded == "" {
		if hasQuery {
			return prefix + "?", nil
		}
		return prefix, nil
	}
	return prefix + "?" + encoded, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// SetClock overrides the clock used for timestamps. Intended for tests.
func (s *Store) SetClock(now func() time.Time) {
	s.now = now
}

func (s *Store) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		sqlBytes, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// UpsertSession inserts or updates the (repo, pane) registration and returns
// the previous pane (empty if none) along with the saved Session row.
func (s *Store) UpsertSession(ctx context.Context, repo, pane string) (string, Session, error) {
	if repo == "" || pane == "" {
		return "", Session{}, fmt.Errorf("repo and pane are required")
	}
	now := s.now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", Session{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prevPane string
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT pane FROM sessions WHERE repo = ?`, repo,
	).Scan(&prevPane); scanErr != nil {
		if !errors.Is(scanErr, sql.ErrNoRows) {
			return "", Session{}, fmt.Errorf("read previous pane: %w", scanErr)
		}
		prevPane = ""
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (repo, pane, registered_at_ns) VALUES (?, ?, ?)
		 ON CONFLICT(repo) DO UPDATE SET
		   pane             = excluded.pane,
		   registered_at_ns = excluded.registered_at_ns`,
		repo, pane, now.UnixNano(),
	); err != nil {
		return "", Session{}, fmt.Errorf("upsert session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", Session{}, fmt.Errorf("commit: %w", err)
	}

	return prevPane, Session{Repo: repo, Pane: pane, RegisteredAt: now}, nil
}

// DeleteSession removes a registration. Returns true if a row was deleted.
func (s *Store) DeleteSession(ctx context.Context, repo string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE repo = ?`, repo)
	if err != nil {
		return false, fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// DeleteSessionByPane removes a registration only if the currently-stored
// pane matches. This avoids clobbering a fresh registration if a session
// re-registers between the time we read the pane and the time we decide to
// prune it. Returns true if a row was actually removed.
func (s *Store) DeleteSessionByPane(ctx context.Context, repo, pane string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE repo = ? AND pane = ?`, repo, pane)
	if err != nil {
		return false, fmt.Errorf("delete session by pane: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// GetSession returns the registered pane for repo, or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, repo string) (Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT repo, pane, registered_at_ns FROM sessions WHERE repo = ?`, repo)
	return scanSession(row)
}

// ListSessions returns all registered sessions ordered by repo. Returns a
// non-nil empty slice when there are no rows.
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, pane, registered_at_ns FROM sessions ORDER BY repo`)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Session, 0)
	for rows.Next() {
		sess, scanErr := scanSession(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(r rowScanner) (Session, error) {
	var (
		s    Session
		regN int64
	)
	if err := r.Scan(&s.Repo, &s.Pane, &regN); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("scan session: %w", err)
	}
	s.RegisteredAt = time.Unix(0, regN).UTC()
	return s, nil
}

// PostMessage inserts a new message and returns the assigned ID and the saved
// timestamp.
func (s *Store) PostMessage(ctx context.Context, from, to, subject, body string) (Message, error) {
	if from == "" || to == "" || subject == "" {
		return Message{}, fmt.Errorf("from, to, and subject are required")
	}
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (from_repo, to_repo, subject, body, posted_at_ns)
		 VALUES (?, ?, ?, ?, ?)`,
		from, to, subject, body, now.UnixNano(),
	)
	if err != nil {
		return Message{}, fmt.Errorf("insert message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("last insert id: %w", err)
	}
	return Message{
		ID: id, From: from, To: to, Subject: subject, Body: body, PostedAt: now,
	}, nil
}

// ListOptions controls pagination of ListMessages.
type ListOptions struct {
	// IncludeRead returns messages already marked read in addition to unread.
	IncludeRead bool
	// Limit caps the number of returned messages. <=0 means use DefaultListLimit.
	// Values above MaxListLimit are clamped to MaxListLimit.
	Limit int
	// SinceID, when >0, returns only messages with id > SinceID. Use the id
	// of the last message you saw to page forward.
	SinceID int64
	// OmitBody requests that message bodies be elided from the result. Useful
	// when paginating through a large inbox where the caller wants to scan
	// subjects before fetching specific message bodies.
	OmitBody bool
}

// ListMessages returns messages addressed to repo, oldest first. Returns a
// non-nil empty slice when there are no rows.
func (s *Store) ListMessages(ctx context.Context, repo string, opts ListOptions) ([]Message, error) {
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = DefaultListLimit
	case limit > MaxListLimit:
		limit = MaxListLimit
	}

	var (
		q    strings.Builder
		args []any
	)
	bodyCol := "body"
	if opts.OmitBody {
		bodyCol = `"" AS body`
	}
	fmt.Fprintf(&q, `SELECT id, from_repo, to_repo, subject, %s, posted_at_ns, read_at_ns
		FROM messages WHERE to_repo = ?`, bodyCol)
	args = append(args, repo)

	if !opts.IncludeRead {
		q.WriteString(` AND read_at_ns IS NULL`)
	}
	if opts.SinceID > 0 {
		q.WriteString(` AND id > ?`)
		args = append(args, opts.SinceID)
	}
	q.WriteString(` ORDER BY posted_at_ns, id LIMIT ?`)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Message, 0, limit)
	for rows.Next() {
		var (
			m        Message
			postedNS int64
			readNS   sql.NullInt64
		)
		if scanErr := rows.Scan(&m.ID, &m.From, &m.To, &m.Subject, &m.Body, &postedNS, &readNS); scanErr != nil {
			return nil, fmt.Errorf("scan message: %w", scanErr)
		}
		m.PostedAt = time.Unix(0, postedNS).UTC()
		if readNS.Valid {
			t := time.Unix(0, readNS.Int64).UTC()
			m.ReadAt = &t
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

// MarkRead sets the read_at timestamp on the given message. Returns true if
// the message exists and was newly marked read; false if already read or not
// found.
func (s *Store) MarkRead(ctx context.Context, id int64) (bool, error) {
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET read_at_ns = ? WHERE id = ? AND read_at_ns IS NULL`,
		now.UnixNano(), id,
	)
	if err != nil {
		return false, fmt.Errorf("update message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// MarkReadBulk marks every supplied id as read in a single SQL UPDATE.
// Returns the number of rows actually updated (already-read ids contribute
// nothing). An empty ids slice is a no-op that returns (0, nil).
func (s *Store) MarkReadBulk(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	now := s.now().UTC()

	// Build "?,?,?,..." placeholder list. SQLite caps a single statement at
	// ~999 bound parameters by default; chunk if necessary.
	const chunk = 500
	var total int64
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(batch)+1)
		args = append(args, now.UnixNano())
		for _, id := range batch {
			args = append(args, id)
		}
		res, err := s.db.ExecContext(ctx,
			`UPDATE messages SET read_at_ns = ?
			   WHERE read_at_ns IS NULL AND id IN (`+placeholders+`)`,
			args...,
		)
		if err != nil {
			return total, fmt.Errorf("bulk update messages: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("rows affected: %w", err)
		}
		total += n
	}
	return total, nil
}
