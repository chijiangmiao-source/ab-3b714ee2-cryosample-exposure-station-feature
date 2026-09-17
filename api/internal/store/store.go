// Package store persists freezer batch state and exposure events in SQLite.
//
// A batch starts inside the cabinet ("in") with zero accumulated exposure.
// Only alternating transitions are allowed: in -> takeout -> out, then
// out -> return -> in. A return adds (return time - matching takeout time)
// seconds to the accumulated exposure. The batch stays usable while the
// accumulated total is <= the allowed seconds; exceeding the limit scraps
// the batch permanently. Every event timestamp must be strictly later than
// the batch's previous timestamp.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Batch states and lifecycle statuses.
const (
	StateIn  = "in"
	StateOut = "out"

	StatusUsable   = "usable"
	StatusScrapped = "scrapped"

	EventTakeout = "takeout"
	EventReturn  = "return"
)

var (
	// ErrNotFound is returned when the barcode does not exist.
	ErrNotFound = errors.New("batch not found")
	// ErrDuplicate is returned when creating a batch with an existing barcode.
	ErrDuplicate = errors.New("barcode already exists")
)

// ConflictError describes a rejected state transition (mapped to HTTP 409).
type ConflictError struct {
	Code    string
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

func conflict(code, format string, args ...any) *ConflictError {
	return &ConflictError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Batch is the persisted freezer-batch aggregate.
type Batch struct {
	Barcode            string
	AllowedSeconds     int64
	State              string // StateIn | StateOut
	Status             string // StatusUsable | StatusScrapped
	AccumulatedSeconds int64
	CreatedAt          time.Time
	LastAt             time.Time  // createdAt, or the last accepted event time
	LastEventType      *string    // nil when no event has happened yet
	LastEventAt        *time.Time // nil when no event has happened yet
	LastTakeoutAt      *time.Time // open takeout time, set while State == StateOut
}

// Event is one accepted takeout/return record.
type Event struct {
	ID           int64
	Barcode      string
	Type         string
	At           time.Time
	DeltaSeconds *int64 // exposure seconds added by a return; NULL for takeouts
}

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the SQLite database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; one connection serializes all
	// transactions so a stale state can never be committed twice.
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS batches (
			barcode             TEXT PRIMARY KEY,
			allowed_seconds     INTEGER NOT NULL CHECK (allowed_seconds > 0),
			state               TEXT NOT NULL CHECK (state IN ('in', 'out')),
			status              TEXT NOT NULL CHECK (status IN ('usable', 'scrapped')),
			accumulated_seconds INTEGER NOT NULL DEFAULT 0 CHECK (accumulated_seconds >= 0),
			created_at          TEXT NOT NULL,
			last_at             TEXT NOT NULL,
			last_event_type     TEXT,
			last_event_at       TEXT,
			last_takeout_at     TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			barcode       TEXT NOT NULL REFERENCES batches (barcode),
			type          TEXT NOT NULL CHECK (type IN ('takeout', 'return')),
			at            TEXT NOT NULL,
			delta_seconds INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_barcode ON events (barcode, id)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// ts formats a timestamp the way it is stored: RFC3339 whole seconds, UTC.
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTS(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// CreateBatch inserts a new batch in the initial state (in cabinet, zero
// accumulated exposure). It returns ErrDuplicate if the barcode exists.
func (s *Store) CreateBatch(ctx context.Context, barcode string, allowedSeconds int64, createdAt time.Time) (*Batch, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO batches
		(barcode, allowed_seconds, state, status, accumulated_seconds, created_at, last_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)`,
		barcode, allowedSeconds, StateIn, StatusUsable, ts(createdAt), ts(createdAt))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite reports "UNIQUE constraint failed" (code 1555/2067).
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

const batchCols = `barcode, allowed_seconds, state, status, accumulated_seconds,
	created_at, last_at, last_event_type, last_event_at, last_takeout_at`

// GetBatch loads a batch by barcode, or ErrNotFound.
func (s *Store) GetBatch(ctx context.Context, barcode string) (*Batch, error) {
	return scanBatch(s.db.QueryRowContext(ctx,
		`SELECT `+batchCols+` FROM batches WHERE barcode = ?`, barcode))
}

func scanBatch(row *sql.Row) (*Batch, error) {
	var b Batch
	var createdAt, lastAt string
	var lastEventType, lastEventAt, lastTakeoutAt sql.NullString
	err := row.Scan(&b.Barcode, &b.AllowedSeconds, &b.State, &b.Status,
		&b.AccumulatedSeconds, &createdAt, &lastAt,
		&lastEventType, &lastEventAt, &lastTakeoutAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if b.CreatedAt, err = parseTS(createdAt); err != nil {
		return nil, err
	}
	if b.LastAt, err = parseTS(lastAt); err != nil {
		return nil, err
	}
	if lastEventType.Valid {
		v := lastEventType.String
		b.LastEventType = &v
	}
	if lastEventAt.Valid {
		t, err := parseTS(lastEventAt.String)
		if err != nil {
			return nil, err
		}
		b.LastEventAt = &t
	}
	if lastTakeoutAt.Valid {
		t, err := parseTS(lastTakeoutAt.String)
		if err != nil {
			return nil, err
		}
		b.LastTakeoutAt = &t
	}
	return &b, nil
}

// ListEvents returns all accepted events of a batch in insertion order.
func (s *Store) ListEvents(ctx context.Context, barcode string) ([]Event, error) {
	if _, err := s.GetBatch(ctx, barcode); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, barcode, type, at, delta_seconds FROM events WHERE barcode = ? ORDER BY id`, barcode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

// LastEvent returns the most recent event of a batch, or nil when none.
func (s *Store) LastEvent(ctx context.Context, barcode string) (*Event, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, barcode, type, at, delta_seconds FROM events WHERE barcode = ? ORDER BY id DESC LIMIT 1`, barcode)
	var ev Event
	var at string
	var delta sql.NullInt64
	err := row.Scan(&ev.ID, &ev.Barcode, &ev.Type, &at, &delta)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := parseTS(at)
	if err != nil {
		return nil, err
	}
	ev.At = t
	if delta.Valid {
		d := delta.Int64
		ev.DeltaSeconds = &d
	}
	return &ev, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(row scanner) (*Event, error) {
	var ev Event
	var at string
	var delta sql.NullInt64
	if err := row.Scan(&ev.ID, &ev.Barcode, &ev.Type, &at, &delta); err != nil {
		return nil, err
	}
	t, err := parseTS(at)
	if err != nil {
		return nil, err
	}
	ev.At = t
	if delta.Valid {
		d := delta.Int64
		ev.DeltaSeconds = &d
	}
	return &ev, nil
}

// ApplyEvent validates and commits one takeout/return event inside a single
// transaction. Rejected transitions write nothing: no event row, no state or
// accumulated change. The conditional UPDATEs pin the previously read state
// so that, even under concurrency, one stale state can succeed at most once.
func (s *Store) ApplyEvent(ctx context.Context, barcode, eventType string, at time.Time) (*Batch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	b, err := scanBatch(tx.QueryRowContext(ctx,
		`SELECT `+batchCols+` FROM batches WHERE barcode = ?`, barcode))
	if err != nil {
		return nil, err
	}

	if !at.After(b.LastAt) {
		return nil, conflict("time_not_monotonic",
			"event time %s must be strictly later than the batch's previous time %s",
			ts(at), ts(b.LastAt))
	}

	var delta *int64
	switch eventType {
	case EventTakeout:
		if b.Status == StatusScrapped {
			return nil, conflict("batch_scrapped", "scrapped batch cannot be taken out again")
		}
		if b.State != StateIn {
			return nil, conflict("invalid_transition", "batch is already out of the cabinet")
		}
		res, err := tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = ?
			WHERE barcode = ? AND state = ? AND status = ? AND last_at = ?`,
			StateOut, ts(at), EventTakeout, ts(at), ts(at),
			barcode, StateIn, StatusUsable, ts(b.LastAt))
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, conflict("invalid_transition", "batch state changed concurrently; retry")
		}

	case EventReturn:
		if b.State != StateOut || b.LastTakeoutAt == nil {
			return nil, conflict("invalid_transition", "batch is not out of the cabinet")
		}
		d := int64(at.Sub(*b.LastTakeoutAt).Seconds())
		newAccumulated := b.AccumulatedSeconds + d
		newStatus := StatusUsable
		if newAccumulated > b.AllowedSeconds {
			newStatus = StatusScrapped
		}
		delta = &d
		res, err := tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, accumulated_seconds = ?, status = ?,
			    last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = NULL
			WHERE barcode = ? AND state = ? AND last_at = ?`,
			StateIn, newAccumulated, newStatus, ts(at), EventReturn, ts(at),
			barcode, StateOut, ts(b.LastAt))
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, conflict("invalid_transition", "batch state changed concurrently; retry")
		}

	default:
		return nil, fmt.Errorf("unknown event type %q", eventType)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (barcode, type, at, delta_seconds) VALUES (?, ?, ?, ?)`,
		barcode, eventType, ts(at), delta); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}
