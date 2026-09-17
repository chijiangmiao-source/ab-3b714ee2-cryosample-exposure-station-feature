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
	// ErrEventNotFound is returned when an event id does not exist for a batch.
	ErrEventNotFound = errors.New("event not found")
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

// Event is one accepted takeout/return record. A revoked event stays in the
// ledger for audit: RevokedAt/RevokeReason are set once it has been undone and
// it no longer contributes to the batch aggregate.
type Event struct {
	ID           int64
	Barcode      string
	Type         string
	At           time.Time
	DeltaSeconds *int64 // exposure seconds added by a return; NULL for takeouts
	RevokedAt    *time.Time
	RevokeReason *string
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
		// Revocation audit trail: at most one row per (revoked) event. The
		// revoked event itself is never deleted, so the ledger plus this table
		// always explains how the aggregate reached its current value.
		`CREATE TABLE IF NOT EXISTS event_revocations (
			event_id   INTEGER PRIMARY KEY REFERENCES events (id),
			barcode    TEXT NOT NULL REFERENCES batches (barcode),
			at         TEXT NOT NULL,
			reason     TEXT NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// Columns added after the initial release; add them idempotently so
	// databases created by older versions keep working.
	for _, col := range []struct{ name, decl string }{
		{"revoked_at", "TEXT"},
		{"revoke_reason", "TEXT"},
	} {
		if err := addColumnIfMissing(ctx, db, "events", col.name, col.decl); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing runs ALTER TABLE ... ADD COLUMN only when the column does
// not exist yet, making the migration safe to re-run.
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, decl string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+decl)
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

// ListEvents returns all events of a batch in insertion order, including
// revoked ones (annotated with their revocation audit fields).
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

// LastEvent returns the most recent active (non-revoked) event of a batch, or
// nil when none remains. Revoked events stay in the ledger but no longer
// describe the current aggregate.
func (s *Store) LastEvent(ctx context.Context, barcode string) (*Event, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE barcode = ? AND revoked_at IS NULL
		 ORDER BY id DESC LIMIT 1`, barcode)
	return scanEventRow(row)
}

const eventCols = `id, barcode, type, at, delta_seconds, revoked_at, revoke_reason`

type scanner interface {
	Scan(dest ...any) error
}

func scanEventRow(row *sql.Row) (*Event, error) {
	var ev Event
	var at string
	var delta sql.NullInt64
	var revokedAt, revokeReason sql.NullString
	err := row.Scan(&ev.ID, &ev.Barcode, &ev.Type, &at, &delta, &revokedAt, &revokeReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := fillEvent(&ev, at, delta, revokedAt, revokeReason); err != nil {
		return nil, err
	}
	return &ev, nil
}

func scanEvent(row scanner) (*Event, error) {
	var ev Event
	var at string
	var delta sql.NullInt64
	var revokedAt, revokeReason sql.NullString
	if err := row.Scan(&ev.ID, &ev.Barcode, &ev.Type, &at, &delta, &revokedAt, &revokeReason); err != nil {
		return nil, err
	}
	if err := fillEvent(&ev, at, delta, revokedAt, revokeReason); err != nil {
		return nil, err
	}
	return &ev, nil
}

func fillEvent(ev *Event, at string, delta sql.NullInt64, revokedAt, revokeReason sql.NullString) error {
	t, err := parseTS(at)
	if err != nil {
		return err
	}
	ev.At = t
	if delta.Valid {
		d := delta.Int64
		ev.DeltaSeconds = &d
	}
	if revokedAt.Valid {
		t, err := parseTS(revokedAt.String)
		if err != nil {
			return err
		}
		ev.RevokedAt = &t
	}
	if revokeReason.Valid {
		r := revokeReason.String
		ev.RevokeReason = &r
	}
	return nil
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

// RevokeEvent undoes the event with the given id when it is the latest
// not-yet-revoked event of a batch, all inside a single transaction. The event
// row is kept and annotated with the revocation time/reason (an
// event_revocations row is written for the audit trail); the batch aggregate
// is rolled back to exactly the state it had before that event:
//
//   - revoking a takeout moves the batch back inside the cabinet;
//   - revoking a return subtracts that exposure, restores the out-of-cabinet
//     state with the matching takeout reopened, and re-evaluates usable vs
//     scrapped against the restored accumulated total.
//
// Only the last active (non-revoked) event may be revoked. The revocation time
// must be strictly later than the batch's current last operation time. All
// rejections roll the whole transaction back: no audit row, no event change,
// no aggregate change. Conditional UPDATEs pin the previously read aggregate
// so concurrent revocations of the same event can commit at most once.
func (s *Store) RevokeEvent(ctx context.Context, barcode string, eventID int64, at time.Time, reason string) (*Batch, error) {
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

	// The targeted event must exist for this batch and still be active.
	var targetRevoked sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT revoked_at FROM events WHERE id = ? AND barcode = ?`,
		eventID, barcode).Scan(&targetRevoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, err
	}
	if targetRevoked.Valid {
		return nil, conflict("already_revoked", "event %d has already been revoked", eventID)
	}

	// Latest active event: revoked rows are skipped, so a non-last event can
	// never be revoked (its successor is still active).
	var ev Event
	var evAt string
	var delta sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT id, type, at, delta_seconds FROM events
		 WHERE barcode = ? AND revoked_at IS NULL ORDER BY id DESC LIMIT 1`,
		barcode).Scan(&ev.ID, &ev.Type, &evAt, &delta)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, conflict("no_revokable_event", "batch has no event that can be revoked")
	}
	if err != nil {
		return nil, err
	}
	if ev.ID != eventID {
		return nil, conflict("not_last_event",
			"only the latest non-revoked event (id %d) can be revoked", ev.ID)
	}
	if ev.At, err = parseTS(evAt); err != nil {
		return nil, err
	}
	if delta.Valid {
		d := delta.Int64
		ev.DeltaSeconds = &d
	}

	// The batch's last_at is the time of the very last active event (or the
	// creation time before any event); a revocation is itself a new recorded
	// operation and must be strictly later.
	if !at.After(b.LastAt) {
		return nil, conflict("time_not_monotonic",
			"revocation time %s must be strictly later than the batch's last operation time %s",
			ts(at), ts(b.LastAt))
	}

	// Predecessor active event, if any: after the revocation it becomes the
	// aggregate's last event again.
	var predType, predAt sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT type, at FROM events
		 WHERE barcode = ? AND revoked_at IS NULL AND id < ? ORDER BY id DESC LIMIT 1`,
		barcode, ev.ID).Scan(&predType, &predAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var res sql.Result
	switch ev.Type {
	case EventTakeout:
		// Undo in -> out: the batch goes back inside, and the open takeout
		// marker disappears. Status cannot be scrapped while out, so it is
		// unchanged. The aggregate's last event reverts to the predecessor
		// (or to no event at all).
		res, err = tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = NULL
			WHERE barcode = ? AND state = ? AND status = ? AND accumulated_seconds = ? AND last_at = ?`,
			StateIn, ts(at), predType, predAt,
			barcode, StateOut, StatusUsable, b.AccumulatedSeconds, ts(b.LastAt))
		if err != nil {
			return nil, err
		}

	case EventReturn:
		// Undo out -> in: subtract exactly the exposure this return added and
		// reopen its matching takeout, which must be the active predecessor.
		if !predType.Valid || predType.String != EventTakeout {
			return nil, conflict("invalid_revocation", "return event has no matching takeout to restore")
		}
		if ev.DeltaSeconds == nil {
			return nil, conflict("invalid_revocation", "return event carries no exposure delta")
		}
		newAccumulated := b.AccumulatedSeconds - *ev.DeltaSeconds
		if newAccumulated < 0 {
			return nil, conflict("invalid_revocation",
				"revoking this return would produce a negative accumulated exposure")
		}
		newStatus := StatusUsable
		if newAccumulated > b.AllowedSeconds {
			newStatus = StatusScrapped
		}
		res, err = tx.ExecContext(ctx, `UPDATE batches
			SET state = ?, accumulated_seconds = ?, status = ?,
			    last_at = ?, last_event_type = ?, last_event_at = ?, last_takeout_at = ?
			WHERE barcode = ? AND state = ? AND status = ? AND accumulated_seconds = ? AND last_at = ?`,
			StateOut, newAccumulated, newStatus, ts(at), predType, predAt, predAt,
			barcode, StateIn, b.Status, b.AccumulatedSeconds, ts(b.LastAt))
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown event type %q", ev.Type)
	}

	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, conflict("invalid_transition", "batch state changed concurrently; retry")
	}

	// Annotate the event and append the audit row. The PRIMARY KEY on
	// event_revocations.event_id makes a double revoke impossible even if two
	// transactions raced past the read above.
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET revoked_at = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`,
		ts(at), reason, ev.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO event_revocations (event_id, barcode, at, reason) VALUES (?, ?, ?, ?)`,
		ev.ID, barcode, ts(at), reason); err != nil {
		if isUniqueViolation(err) {
			return nil, conflict("already_revoked", "event has already been revoked")
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBatch(ctx, barcode)
}
