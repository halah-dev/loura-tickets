// Package store persists tickets in SQLite.
//
// Two things carry the ticket lifecycle. Claim is an atomic lease: a worker
// takes a pending ticket by writing a lease expiry in the same statement that
// selects it, so two workers cannot take the same one and a dead worker does
// not strand it. SaveResult and Fail are both conditional on the ticket still
// being pending, so a ticket reaches a terminal state exactly once even if a
// lease expired while a slow classification was still running.
package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/halah-dev/loura-tickets/internal/ticket"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a ticket id does not exist.
var ErrNotFound = errors.New("ticket not found")

// Store is a SQLite-backed ticket store.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS tickets (
	id               TEXT PRIMARY KEY,
	subject          TEXT NOT NULL,
	body             TEXT NOT NULL,
	status           TEXT NOT NULL,
	category         TEXT,
	priority         TEXT,
	summary          TEXT,
	attempts         INTEGER NOT NULL DEFAULT 0,
	error            TEXT,
	prompt_version   INTEGER,
	review_required  INTEGER NOT NULL DEFAULT 0,
	review_reasons   TEXT,
	lease_expires_at INTEGER,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	classified_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tickets_claim ON tickets(status, lease_expires_at);
CREATE INDEX IF NOT EXISTS idx_tickets_list  ON tickets(category, priority, created_at, id);
CREATE INDEX IF NOT EXISTS idx_tickets_review ON tickets(review_required, created_at, id);
`

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	// WAL allows one writer but many concurrent readers, so the pool is sized to
	// let API reads proceed while workers are writing; busy_timeout absorbs
	// writer contention. Measured: a Get under four concurrent writers costs
	// ~19us here against ~50us pinned to one connection.
	db.SetMaxOpenConns(8)

	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		db.Close()
		// SQLite reports a path it cannot open as "out of memory (14)", which
		// sends you looking in the wrong place entirely. Naming the file is the
		// difference between a minute and an afternoon.
		return nil, fmt.Errorf("opening database %s (is the directory writable?): %w", path, err)
	}
	if err := addColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// addColumns brings an older database up to date. CREATE TABLE IF NOT EXISTS
// does nothing to a table that already exists, and SQLite has no ADD COLUMN IF
// NOT EXISTS, so check the table rather than swallowing the error.
func addColumns(db *sql.DB) error {
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('tickets')`)
	if err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	defer rows.Close()

	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("inspect schema: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}

	for _, c := range []struct{ name, ddl string }{
		{"review_required", "ALTER TABLE tickets ADD COLUMN review_required INTEGER NOT NULL DEFAULT 0"},
		{"review_reasons", "ALTER TABLE tickets ADD COLUMN review_reasons TEXT"},
	} {
		if have[c.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// NewTicket is the input to Create. A struct because three fields are strings
// and transposing two of them would compile.
type NewTicket struct {
	ID      string
	Subject string
	Body    string

	// ReviewRequired and ReviewReasons come from the caller's ingest-time
	// content check. The store does not look at ticket text itself.
	ReviewRequired bool
	ReviewReasons  []string
}

// Create inserts a new pending ticket. It is idempotent on id: submitting the
// same id twice returns the ticket that already exists and created=false, and
// does not reset its status or re-run classification.
func (s *Store) Create(ctx context.Context, nt NewTicket) (t ticket.Ticket, created bool, err error) {
	now := time.Now().UTC()

	var reasons any
	if len(nt.ReviewReasons) > 0 {
		reasons = strings.Join(nt.ReviewReasons, ",")
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tickets (id, subject, body, status, attempts, created_at, updated_at,
		                     review_required, review_reasons)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		nt.ID, nt.Subject, nt.Body, string(ticket.StatusPending),
		now.UnixNano(), now.UnixNano(), nt.ReviewRequired, reasons)
	if err != nil {
		return ticket.Ticket{}, false, fmt.Errorf("insert ticket: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ticket.Ticket{}, false, fmt.Errorf("insert ticket: %w", err)
	}

	existing, err := s.Get(ctx, nt.ID)
	if err != nil {
		return ticket.Ticket{}, false, err
	}
	return existing, n == 1, nil
}

// Get returns one ticket by id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (ticket.Ticket, error) {
	row := s.db.QueryRowContext(ctx, selectColumns+` FROM tickets WHERE id = ?`, id)
	t, err := scanTicket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ticket.Ticket{}, ErrNotFound
	}
	return t, err
}

// Claim takes the oldest unleased pending ticket, leases it, and counts the
// attempt. ErrNotFound means there is no work.
func (s *Store) Claim(ctx context.Context, lease time.Duration) (ticket.Ticket, error) {
	now := time.Now().UTC()
	row := s.db.QueryRowContext(ctx, `
		UPDATE tickets
		   SET lease_expires_at = ?, attempts = attempts + 1, updated_at = ?
		 WHERE id = (
		       SELECT id FROM tickets
		        WHERE status = ?
		          AND (lease_expires_at IS NULL OR lease_expires_at <= ?)
		        ORDER BY created_at, id
		        LIMIT 1
		 )
		RETURNING `+columns,
		now.Add(lease).UnixNano(), now.UnixNano(),
		string(ticket.StatusPending), now.UnixNano())

	t, err := scanTicket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ticket.Ticket{}, ErrNotFound
	}
	return t, err
}

// SaveResult stores a validated classification. A no-op if the ticket is no
// longer pending, which makes a duplicated in-flight classification harmless.
func (s *Store) SaveResult(ctx context.Context, id string, c ticket.Classification, promptVersion int) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE tickets
		   SET status = ?, category = ?, priority = ?, summary = ?,
		       prompt_version = ?, error = NULL, lease_expires_at = NULL,
		       updated_at = ?, classified_at = ?
		 WHERE id = ? AND status = ?`,
		string(ticket.StatusClassified), string(c.Category), string(c.Priority), c.Summary,
		promptVersion, now.UnixNano(), now.UnixNano(), id, string(ticket.StatusPending))
	if err != nil {
		return fmt.Errorf("save classification: %w", err)
	}
	return nil
}

// Fail moves a still-pending ticket to failed with a reason.
func (s *Store) Fail(ctx context.Context, id, reason string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE tickets
		   SET status = ?, error = ?, lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND status = ?`,
		string(ticket.StatusFailed), reason, now.UnixNano(), id, string(ticket.StatusPending))
	if err != nil {
		return fmt.Errorf("fail ticket: %w", err)
	}
	return nil
}

// Release hands a ticket back without a verdict, claimable again at notBefore.
// Used for both retry backoff and the shutdown hand-back.
func (s *Store) Release(ctx context.Context, id string, notBefore time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tickets
		   SET lease_expires_at = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		notBefore.UnixNano(), time.Now().UTC().UnixNano(), id, string(ticket.StatusPending))
	if err != nil {
		return fmt.Errorf("release ticket: %w", err)
	}
	return nil
}

// ListFilter selects and pages through tickets. A zero Category or Priority
// means "any". Limit is clamped by the caller.
type ListFilter struct {
	Category *ticket.Category
	Priority *ticket.Priority
	Status   *ticket.Status
	// ReviewRequired filters to, or away from, tickets flagged at ingest.
	ReviewRequired *bool
	Limit          int
	Cursor         string
}

// Page is one page of list results.
type Page struct {
	Tickets    []ticket.Ticket
	NextCursor string
}

// List pages oldest first, keyset on (created_at, id) rather than offset, so a
// ticket arriving mid-traversal cannot shift a page boundary and skip a row.
func (s *Store) List(ctx context.Context, f ListFilter) (Page, error) {
	var where []string
	var args []any

	if f.Category != nil {
		where = append(where, "category = ?")
		args = append(args, string(*f.Category))
	}
	if f.Priority != nil {
		where = append(where, "priority = ?")
		args = append(args, string(*f.Priority))
	}
	if f.Status != nil {
		where = append(where, "status = ?")
		args = append(args, string(*f.Status))
	}
	if f.ReviewRequired != nil {
		where = append(where, "review_required = ?")
		args = append(args, *f.ReviewRequired)
	}
	if f.Cursor != "" {
		createdAt, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
		where = append(where, "(created_at > ? OR (created_at = ? AND id > ?))")
		args = append(args, createdAt, createdAt, id)
	}

	q := selectColumns + " FROM tickets"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// One extra row tells us whether another page exists, without a count query.
	q += " ORDER BY created_at, id LIMIT ?"
	args = append(args, f.Limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return Page{}, fmt.Errorf("list tickets: %w", err)
	}
	defer rows.Close()

	page := Page{Tickets: []ticket.Ticket{}}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return Page{}, err
		}
		page.Tickets = append(page.Tickets, t)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("list tickets: %w", err)
	}

	if len(page.Tickets) > f.Limit {
		page.Tickets = page.Tickets[:f.Limit]
		last := page.Tickets[len(page.Tickets)-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// ErrBadCursor is returned for a cursor the client did not get from us.
var ErrBadCursor = errors.New("malformed cursor")

func encodeCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(createdAt.UnixNano(), 10) + "|" + id))
}

func decodeCursor(c string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	nanos, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return 0, "", ErrBadCursor
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	return n, id, nil
}

const columns = `id, subject, body, status, category, priority, summary,
	attempts, error, created_at, updated_at, classified_at,
	review_required, review_reasons`

const selectColumns = `SELECT ` + columns

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanTicket(sc scanner) (ticket.Ticket, error) {
	var (
		t                                 ticket.Ticket
		status                            string
		category, priority, summary, errS sql.NullString
		createdAt, updatedAt              int64
		classifiedAt                      sql.NullInt64
		reviewReasons                     sql.NullString
	)
	err := sc.Scan(&t.ID, &t.Subject, &t.Body, &status, &category, &priority, &summary,
		&t.Attempts, &errS, &createdAt, &updatedAt, &classifiedAt,
		&t.ReviewRequired, &reviewReasons)
	if err != nil {
		return ticket.Ticket{}, err
	}

	t.Status = ticket.Status(status)
	if category.Valid {
		c := ticket.Category(category.String)
		t.Category = &c
	}
	if priority.Valid {
		p := ticket.Priority(priority.String)
		t.Priority = &p
	}
	if summary.Valid {
		t.Summary = &summary.String
	}
	if errS.Valid {
		t.Error = &errS.String
	}
	if reviewReasons.Valid && reviewReasons.String != "" {
		t.ReviewReasons = strings.Split(reviewReasons.String, ",")
	}
	t.CreatedAt = time.Unix(0, createdAt).UTC()
	t.UpdatedAt = time.Unix(0, updatedAt).UTC()
	if classifiedAt.Valid {
		ts := time.Unix(0, classifiedAt.Int64).UTC()
		t.ClassifiedAt = &ts
	}
	return t, nil
}
