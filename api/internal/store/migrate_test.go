package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"timingstation/api/internal/store"
)

// openLegacyDB creates a database using the pre-revocation schema (events has
// no revoked columns and there is no event_revocations table), then seeds one
// in-cabinet batch.
func openLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE batches (
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
	_, err = db.Exec(`CREATE TABLE events (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		barcode       TEXT NOT NULL REFERENCES batches (barcode),
		type          TEXT NOT NULL CHECK (type IN ('takeout', 'return')),
		at            TEXT NOT NULL,
		delta_seconds INTEGER
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE INDEX idx_events_barcode ON events (barcode, id)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO batches
		(barcode, allowed_seconds, state, status, accumulated_seconds, created_at, last_at)
		VALUES ('OLD-1', 100, 'in', 'usable', 0, '2026-09-13T08:00:00Z', '2026-09-13T08:00:00Z')`)
	require.NoError(t, err)
}

func TestMigratesLegacySchemaAndSupportsRevocation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	openLegacyDB(t, path)

	// Opening with the new code must add the audit columns/table in place.
	st, err := store.Open(ctx, path)
	require.NoError(t, err)
	defer st.Close()

	b, err := st.GetBatch(ctx, "OLD-1")
	require.NoError(t, err)
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)

	t0, _ := time.Parse(time.RFC3339, "2026-09-13T08:00:10Z")
	b, err = st.ApplyEvent(ctx, "OLD-1", store.EventTakeout, t0)
	require.NoError(t, err)
	assert.Equal(t, "out", b.State)

	rt, _ := time.Parse(time.RFC3339, "2026-09-13T08:00:20Z")
	b, err = st.RevokeEvent(ctx, "OLD-1", 1, rt, "legacy db revoke")
	require.NoError(t, err)
	assert.Equal(t, "in", b.State)

	evs, err := st.ListEvents(ctx, "OLD-1")
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.NotNil(t, evs[0].RevokedAt)
	assert.Equal(t, "2026-09-13T08:00:20Z", evs[0].RevokedAt.UTC().Format(time.RFC3339))
	require.NotNil(t, evs[0].RevokeReason)
	assert.Equal(t, "legacy db revoke", *evs[0].RevokeReason)

	// Migration is idempotent: reopening must not fail.
	require.NoError(t, st.Close())
	st2, err := store.Open(ctx, path)
	require.NoError(t, err)
	defer st2.Close()
}
