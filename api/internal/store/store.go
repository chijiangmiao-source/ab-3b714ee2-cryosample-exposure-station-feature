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

// Event is one accepted takeout/return record. A mis-scanned event can be
// undone: the row is kept for audit and carries the undo time and reason.
type Event struct {
	ID           int64
	Barcode      string
	Type         string
	At           time.Time
	DeltaSeconds *int64     // exposure seconds added by a return; NULL for takeouts
	UndoneAt     *time.Time // set when the event was undone; NULL while active
	UndoReason   *string    // operator-supplied reason recorded with the undo
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
			delta_seconds INTEGER,
			undone_at     TEXT,
			undo_reason   TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_barcode ON events (barcode, id)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// Databases created before the undo feature lack the audit columns;
	// add them idempotently (SQLite has no ADD COLUMN IF NOT EXISTS).
	if err := ensureColumn(ctx, db, "events", "undone_at", "undone_at TEXT"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "events", "undo_reason", "undo_reason TEXT"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds column definition to table when it is missing.
func ensureColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+definition)
	return err
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

const eventCols = `id, barcode, type, at, delta_seconds, undone_at, undo_reason`

// ListEvents returns all accepted events of a batch in insertion order,
// including undone ones (the audit trail is never removed).
func (s *Store) ListEvents(ctx context.Context, barcode string) ([]Event, error) {
	if _, err := s.GetBatch(ctx, barcode); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE barcode = ? ORDER BY id`, barcode)
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

// LastEvent returns the most recent still-active (non-undone) event of a
// batch, or nil when none. Undone events no longer describe the batch state.
func (s *Store) LastEvent(ctx context.Context, barcode string) (*Event, error) {
	ev, err := scanEvent(s.db.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE barcode = ? AND undone_at IS NULL ORDER BY id DESC LIMIT 1`, barcode))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(row scanner) (*Event, error) {
	var ev Event
	var at string
	var delta sql.NullInt64
	var undoneAt, undoReason sql.NullString
	if err := row.Scan(&ev.ID, &ev.Barcode, &ev.Type, &at, &delta, &undoneAt, &undoReason); err != nil {
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
	if undoneAt.Valid {
		u, err := parseTS(undoneAt.String)
		if err != nil {
			return nil, err
		}
		ev.UndoneAt = &u
	}
	if undoReason.Valid {
		r := undoReason.String
		ev.UndoReason = &r
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

// UndoEvent reverses the batch's most recent still-active event inside a
// single transaction: the event row keeps its audit trail (undone_at +
// reason) and the batch aggregate is restored to its state before that
// event. Undoing a takeout puts the batch back into the cabinet; undoing a
// return puts it back outside, subtracts the exposure that return had added
// and re-judges usability from the restored total. Only the last non-undone
// event may be undone; rejected undos write nothing.
func (s *Store) UndoEvent(ctx context.Context, barcode string, eventID int64, at time.Time, reason string) (*Batch, error) {
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

	ev, err := scanEvent(tx.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE id = ? AND barcode = ?`, eventID, barcode))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ev.UndoneAt != nil {
		return nil, conflict("already_undone", "event %d was already undone", ev.ID)
	}

	// Only the current last non-undone event may be undone.
	var lastID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM events WHERE barcode = ? AND undone_at IS NULL ORDER BY id DESC LIMIT 1`,
		barcode).Scan(&lastID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, conflict("not_last_event", "event %d is not the latest active event", ev.ID)
	}
	if err != nil {
		return nil, err
	}
	if lastID != ev.ID {
		return nil, conflict("not_last_event",
			"event %d is not the latest active event (only event %d can be undone)", ev.ID, lastID)
	}

	// The undo cannot be dated before the batch's last operation.
	if at.Before(b.LastAt) {
		return nil, conflict("time_not_monotonic",
			"undo time %s must not be earlier than the batch's last operation time %s",
			ts(at), ts(b.LastAt))
	}

	// The previous still-active event defines the state to restore.
	prev, err := scanEvent(tx.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE barcode = ? AND undone_at IS NULL AND id < ? ORDER BY id DESC LIMIT 1`,
		barcode, ev.ID))
	if errors.Is(err, sql.ErrNoRows) {
		prev = nil
	} else if err != nil {
		return nil, err
	}

	// Restore the "last" markers to the previous active event (or creation).
	lastAt := b.CreatedAt
	var lastType, lastEventAt sql.NullString
	if prev != nil {
		lastAt = prev.At
		lastType = sql.NullString{String: prev.Type, Valid: true}
		lastEventAt = sql.NullString{String: ts(prev.At), Valid: true}
	}

	switch ev.Type {
	case EventTakeout:
		res, err := tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = NULL
			WHERE barcode = ? AND state = ? AND last_at = ?`,
			StateIn, ts(lastAt), lastType, lastEventAt,
			barcode, StateOut, ts(b.LastAt))
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, conflict("invalid_transition", "batch state changed concurrently; retry")
		}

	case EventReturn:
		// The event before a return is always its matching takeout.
		if prev == nil || prev.Type != EventTakeout {
			return nil, fmt.Errorf("return event %d has no matching takeout", ev.ID)
		}
		if ev.DeltaSeconds == nil {
			return nil, fmt.Errorf("return event %d has no recorded exposure", ev.ID)
		}
		newAccumulated := b.AccumulatedSeconds - *ev.DeltaSeconds
		newStatus := StatusUsable
		if newAccumulated > b.AllowedSeconds {
			newStatus = StatusScrapped
		}
		res, err := tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, accumulated_seconds = ?, status = ?,
			    last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = ?
			WHERE barcode = ? AND state = ? AND last_at = ?`,
			StateOut, newAccumulated, newStatus,
			ts(lastAt), lastType, lastEventAt, ts(prev.At),
			barcode, StateIn, ts(b.LastAt))
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return nil, conflict("invalid_transition", "batch state changed concurrently; retry")
		}

	default:
		return nil, fmt.Errorf("unknown event type %q", ev.Type)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE events SET undone_at = ?, undo_reason = ? WHERE id = ? AND undone_at IS NULL`,
		ts(at), reason, ev.ID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, conflict("already_undone", "event %d was already undone", ev.ID)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}
