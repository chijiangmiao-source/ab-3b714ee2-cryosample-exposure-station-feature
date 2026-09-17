package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"timingstation/api/internal/store"
)

// Open must migrate databases created before the undo feature: the audit
// columns are added and the pre-existing rows keep working.
func TestOpenMigratesPreUndoSchema(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/old.db"

	// Build a database with the pre-undo schema by hand.
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `CREATE TABLE batches (
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
	)`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `CREATE TABLE events (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		barcode       TEXT NOT NULL REFERENCES batches (barcode),
		type          TEXT NOT NULL CHECK (type IN ('takeout', 'return')),
		at            TEXT NOT NULL,
		delta_seconds INTEGER
	)`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO batches
		(barcode, allowed_seconds, state, status, accumulated_seconds, created_at, last_at, last_event_type, last_event_at, last_takeout_at)
		VALUES ('OLD-1', 100, 'out', 'usable', 0, '2026-09-13T08:00:00Z', '2026-09-13T08:00:10Z', 'takeout', '2026-09-13T08:00:10Z', '2026-09-13T08:00:10Z')`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO events (barcode, type, at) VALUES ('OLD-1', 'takeout', '2026-09-13T08:00:10Z')`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Opening with the current code adds the audit columns.
	st, err := store.Open(ctx, dbPath)
	require.NoError(t, err)
	defer st.Close()

	events, err := st.ListEvents(ctx, "OLD-1")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Nil(t, events[0].UndoneAt, "pre-existing rows start active")
	assert.Nil(t, events[0].UndoReason)

	// And the undo flow works on the migrated row.
	undoAt, err := time.Parse(time.RFC3339, "2026-09-13T08:00:20Z")
	require.NoError(t, err)
	b, err := st.UndoEvent(ctx, "OLD-1", events[0].ID, undoAt, "误扫取出")
	require.NoError(t, err)
	assert.Equal(t, store.StateIn, b.State)

	events, err = st.ListEvents(ctx, "OLD-1")
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UndoneAt)
	assert.Equal(t, "误扫取出", *events[0].UndoReason)
}
