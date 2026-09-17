package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"timingstation/api/internal/app"
	"timingstation/api/internal/store"
)

// --- helpers ---------------------------------------------------------------

type batchResp struct {
	Barcode            string `json:"barcode"`
	AllowedSeconds     int64  `json:"allowedSeconds"`
	State              string `json:"state"`
	Status             string `json:"status"`
	Usable             bool   `json:"usable"`
	AccumulatedSeconds int64  `json:"accumulatedSeconds"`
	RemainingSeconds   int64  `json:"remainingSeconds"`
	CreatedAt          string `json:"createdAt"`
	LastEvent          *struct {
		ID           int64   `json:"id"`
		Type         string  `json:"type"`
		At           string  `json:"at"`
		DeltaSeconds *int64  `json:"deltaSeconds"`
		RevokedAt    *string `json:"revokedAt"`
		RevokeReason *string `json:"revokeReason"`
	} `json:"lastEvent"`
}

type errResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type eventsResp struct {
	Events []struct {
		ID           int64   `json:"id"`
		Type         string  `json:"type"`
		At           string  `json:"at"`
		DeltaSeconds *int64  `json:"deltaSeconds"`
		RevokedAt    *string `json:"revokedAt"`
		RevokeReason *string `json:"revokeReason"`
	} `json:"events"`
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(app.NewRouter(st))
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, raw
}

func createBatch(t *testing.T, srv *httptest.Server, barcode string, allowed int64, createdAt string) (int, batchResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches", map[string]any{
		"barcode": barcode, "allowedSeconds": allowed, "createdAt": createdAt,
	})
	var b batchResp
	if code == http.StatusCreated {
		require.NoError(t, json.Unmarshal(raw, &b))
	}
	return code, b
}

func postEvent(t *testing.T, srv *httptest.Server, barcode, typ, at string) (int, batchResp, errResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches/"+barcode+"/events", map[string]string{
		"type": typ, "at": at,
	})
	var b batchResp
	var e errResp
	if code == http.StatusCreated {
		require.NoError(t, json.Unmarshal(raw, &b))
	} else {
		require.NoError(t, json.Unmarshal(raw, &e))
	}
	return code, b, e
}

func getBatch(t *testing.T, srv *httptest.Server, barcode string) batchResp {
	t.Helper()
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/"+barcode, nil)
	require.Equal(t, http.StatusOK, code)
	var b batchResp
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

func listEvents(t *testing.T, srv *httptest.Server, barcode string) eventsResp {
	t.Helper()
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/"+barcode+"/events", nil)
	require.Equal(t, http.StatusOK, code)
	var ev eventsResp
	require.NoError(t, json.Unmarshal(raw, &ev))
	return ev
}

// --- creation & validation ---------------------------------------------------

func TestCreateBatchInitialState(t *testing.T) {
	srv := newServer(t)
	code, b := createBatch(t, srv, "B-1", 3600, "2026-09-13T08:00:00Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "B-1", b.Barcode)
	assert.Equal(t, int64(3600), b.AllowedSeconds)
	assert.Equal(t, "in", b.State, "batch must start inside the cabinet")
	assert.Equal(t, "usable", b.Status)
	assert.True(t, b.Usable)
	assert.Equal(t, int64(0), b.AccumulatedSeconds, "accumulated exposure starts at zero")
	assert.Equal(t, int64(3600), b.RemainingSeconds)
	assert.Equal(t, "2026-09-13T08:00:00Z", b.CreatedAt)
	assert.Nil(t, b.LastEvent)
}

func TestCreateBatchDuplicateBarcode(t *testing.T) {
	srv := newServer(t)
	code, _ := createBatch(t, srv, "B-DUP", 10, "2026-09-13T08:00:00Z")
	require.Equal(t, http.StatusCreated, code)
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches", map[string]any{
		"barcode": "B-DUP", "allowedSeconds": 20, "createdAt": "2026-09-13T09:00:00Z",
	})
	assert.Equal(t, http.StatusConflict, code)
	var e errResp
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "duplicate_barcode", e.Error.Code)
}

func TestCreateBatchValidation(t *testing.T) {
	srv := newServer(t)
	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{"empty barcode", map[string]any{"barcode": "  ", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00Z"}, "barcode_required"},
		{"zero allowed", map[string]any{"barcode": "V-1", "allowedSeconds": 0, "createdAt": "2026-09-13T08:00:00Z"}, "invalid_allowed_seconds"},
		{"negative allowed", map[string]any{"barcode": "V-2", "allowedSeconds": -5, "createdAt": "2026-09-13T08:00:00Z"}, "invalid_allowed_seconds"},
		{"fractional seconds", map[string]any{"barcode": "V-3", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00.5Z"}, "invalid_time"},
		{"offset not Z", map[string]any{"barcode": "V-4", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00+08:00"}, "invalid_time"},
		{"space separator", map[string]any{"barcode": "V-5", "allowedSeconds": 10, "createdAt": "2026-09-13 08:00:00"}, "invalid_time"},
		{"missing Z", map[string]any{"barcode": "V-6", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00"}, "invalid_time"},
		{"garbage", map[string]any{"barcode": "V-7", "allowedSeconds": 10, "createdAt": "tomorrow"}, "invalid_time"},
		{"impossible date", map[string]any{"barcode": "V-8", "allowedSeconds": 10, "createdAt": "2026-13-40T25:61:61Z"}, "invalid_time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches", tc.body)
			require.Equal(t, http.StatusBadRequest, code, "body: %s", raw)
			var e errResp
			require.NoError(t, json.Unmarshal(raw, &e))
			assert.Equal(t, tc.code, e.Error.Code)
		})
	}
}

func TestGetUnknownBatch(t *testing.T) {
	srv := newServer(t)
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/batches/NOPE", nil)
	assert.Equal(t, http.StatusNotFound, code)
	var e errResp
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "not_found", e.Error.Code)
}

// --- transition rules ---------------------------------------------------------

func TestTakeoutReturnHappyPath(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-1", 100, "2026-09-13T08:00:00Z")

	code, b, _ := postEvent(t, srv, "F-1", "takeout", "2026-09-13T08:00:10Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "takeout", b.LastEvent.Type)
	assert.Equal(t, "2026-09-13T08:00:10Z", b.LastEvent.At)
	assert.Nil(t, b.LastEvent.DeltaSeconds)

	code, b, _ = postEvent(t, srv, "F-1", "return", "2026-09-13T08:00:40Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(30), b.AccumulatedSeconds, "return adds return-minus-takeout seconds")
	assert.Equal(t, int64(70), b.RemainingSeconds)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "return", b.LastEvent.Type)
	require.NotNil(t, b.LastEvent.DeltaSeconds)
	assert.Equal(t, int64(30), *b.LastEvent.DeltaSeconds)
	assert.Equal(t, "usable", b.Status)
}

func TestConsecutiveTakeoutRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-2", 100, "2026-09-13T08:00:00Z")
	code, _, _ := postEvent(t, srv, "F-2", "takeout", "2026-09-13T08:00:10Z")
	require.Equal(t, http.StatusCreated, code)

	code, _, e := postEvent(t, srv, "F-2", "takeout", "2026-09-13T08:00:20Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "invalid_transition", e.Error.Code)

	// The failed takeout must not have written anything.
	b := getBatch(t, srv, "F-2")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, "2026-09-13T08:00:10Z", b.LastEvent.At)
	assert.Len(t, listEvents(t, srv, "F-2").Events, 1)
}

func TestReturnWithoutTakeoutRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-3", 100, "2026-09-13T08:00:00Z")

	code, _, e := postEvent(t, srv, "F-3", "return", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "invalid_transition", e.Error.Code)

	b := getBatch(t, srv, "F-3")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	assert.Empty(t, listEvents(t, srv, "F-3").Events)
}

func TestDuplicateReturnRejectedAndNotDoubleCounted(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-4", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "F-4", "takeout", "2026-09-13T08:00:10Z")
	code, b, _ := postEvent(t, srv, "F-4", "return", "2026-09-13T08:00:20Z")
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, int64(10), b.AccumulatedSeconds)

	// Paper-log bug scenario: the same return recorded a second time.
	code, _, e := postEvent(t, srv, "F-4", "return", "2026-09-13T08:00:30Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "invalid_transition", e.Error.Code)

	b = getBatch(t, srv, "F-4")
	assert.Equal(t, int64(10), b.AccumulatedSeconds, "duplicate return must not add exposure twice")
	assert.Len(t, listEvents(t, srv, "F-4").Events, 2)
}

func TestOutOfOrderTimeRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-5", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "F-5", "takeout", "2026-09-13T08:00:10Z")

	// Equal to the previous timestamp.
	code, _, e := postEvent(t, srv, "F-5", "return", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	// Strictly before the previous timestamp.
	code, _, e = postEvent(t, srv, "F-5", "return", "2026-09-13T08:00:05Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	// Equal to createdAt is also rejected for the very first event.
	createBatch(t, srv, "F-5b", 100, "2026-09-13T08:00:00Z")
	code, _, e = postEvent(t, srv, "F-5b", "takeout", "2026-09-13T08:00:00Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	b := getBatch(t, srv, "F-5")
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	assert.Len(t, listEvents(t, srv, "F-5").Events, 1)
}

func TestEventTimeFormatRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "F-6", 100, "2026-09-13T08:00:00Z")
	for _, at := range []string{
		"2026-09-13T08:00:10",       // missing Z
		"2026-09-13T08:00:10+08:00", // offset
		"2026-09-13T08:00:10.000Z",  // fractional seconds
		"2026-09-13 08:00:10",       // space separator
		"not-a-time",
	} {
		code, _, e := postEvent(t, srv, "F-6", "takeout", at)
		assert.Equal(t, http.StatusBadRequest, code, "at=%q", at)
		assert.Equal(t, "invalid_time", e.Error.Code, "at=%q", at)
	}
	code, _, e := postEvent(t, srv, "F-6", "freeze", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_event_type", e.Error.Code)
}

// --- exposure limit boundaries -------------------------------------------------

func TestBoundaryReturnExactlyAtLimitStaysUsable(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-1", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-1", "takeout", "2026-09-13T08:00:05Z")
	code, b, _ := postEvent(t, srv, "L-1", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(10), b.AccumulatedSeconds)
	assert.Equal(t, int64(0), b.RemainingSeconds)
	assert.Equal(t, "usable", b.Status, "accumulated == limit must remain usable")
	assert.True(t, b.Usable)

	// And the batch can still go out and come back within its remaining 0s...
	// any positive exposure now exceeds the limit.
	postEvent(t, srv, "L-1", "takeout", "2026-09-13T08:00:20Z")
	code, b, _ = postEvent(t, srv, "L-1", "return", "2026-09-13T08:00:21Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(11), b.AccumulatedSeconds)
	assert.Equal(t, "scrapped", b.Status)
	assert.False(t, b.Usable)
}

func TestOverLimitReturnScrapsImmediately(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-2", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-2", "takeout", "2026-09-13T08:00:05Z")
	code, b, _ := postEvent(t, srv, "L-2", "return", "2026-09-13T08:00:16Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(11), b.AccumulatedSeconds)
	assert.Equal(t, int64(-1), b.RemainingSeconds)
	assert.Equal(t, "scrapped", b.Status, "exceeding the limit scraps the batch on return")
}

func TestScrappedBatchCannotBeTakenOut(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-3", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-3", "takeout", "2026-09-13T08:00:05Z")
	postEvent(t, srv, "L-3", "return", "2026-09-13T08:00:16Z")

	code, _, e := postEvent(t, srv, "L-3", "takeout", "2026-09-13T08:00:20Z")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "batch_scrapped", e.Error.Code)

	b := getBatch(t, srv, "L-3")
	assert.Equal(t, "in", b.State)
	assert.Len(t, listEvents(t, srv, "L-3").Events, 2, "rejected takeout must not write an event")
}

func TestAccumulationAcrossMultipleCycles(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "L-4", 60, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "L-4", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "L-4", "return", "2026-09-13T08:00:30Z") // +20
	postEvent(t, srv, "L-4", "takeout", "2026-09-13T08:01:00Z")
	postEvent(t, srv, "L-4", "return", "2026-09-13T08:01:25Z") // +25
	b := getBatch(t, srv, "L-4")
	assert.Equal(t, int64(45), b.AccumulatedSeconds)
	assert.Equal(t, int64(15), b.RemainingSeconds)
	assert.Equal(t, "usable", b.Status)
	assert.Len(t, listEvents(t, srv, "L-4").Events, 4)
}

// --- persistence ------------------------------------------------------------

func TestStateSurvivesReopen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	st, err := store.Open(context.Background(), dbPath)
	require.NoError(t, err)
	srv := httptest.NewServer(app.NewRouter(st))
	createBatch(t, srv, "P-1", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "P-1", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "P-1", "return", "2026-09-13T08:00:40Z")
	srv.Close()
	require.NoError(t, st.Close())

	st2, err := store.Open(context.Background(), dbPath)
	require.NoError(t, err)
	defer st2.Close()
	srv2 := httptest.NewServer(app.NewRouter(st2))
	defer srv2.Close()

	b := getBatch(t, srv2, "P-1")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(30), b.AccumulatedSeconds)
	assert.Equal(t, "return", b.LastEvent.Type)
	assert.Len(t, listEvents(t, srv2, "P-1").Events, 2)
}

// --- concurrency --------------------------------------------------------------

// postEventRaw is a goroutine-safe variant returning (status, errorCode).
func postEventRaw(srv *httptest.Server, barcode, typ, at string) (int, string) {
	raw, err := json.Marshal(map[string]string{"type": typ, "at": at})
	if err != nil {
		return -1, err.Error()
	}
	res, err := http.Post(srv.URL+"/api/batches/"+barcode+"/events", "application/json", bytes.NewReader(raw))
	if err != nil {
		return -1, err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusCreated {
		var e errResp
		if json.Unmarshal(body, &e) == nil {
			return res.StatusCode, e.Error.Code
		}
	}
	return res.StatusCode, ""
}

func TestConcurrentTakeoutOnlyOneWins(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "C-1", 100, "2026-09-13T08:00:00Z")

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := postEventRaw(srv, "C-1", "takeout", "2026-09-13T08:00:10Z")
			codes[i] = code
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, wins, "the same old state may succeed at most once")

	b := getBatch(t, srv, "C-1")
	assert.Equal(t, "out", b.State)
	assert.Len(t, listEvents(t, srv, "C-1").Events, 1)
}

func TestConcurrentReturnCountsExposureOnce(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "C-2", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "C-2", "takeout", "2026-09-13T08:00:10Z")

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := postEventRaw(srv, "C-2", "return", "2026-09-13T08:00:20Z")
			codes[i] = code
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, wins, "racing returns: exactly one may commit")

	b := getBatch(t, srv, "C-2")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(10), b.AccumulatedSeconds, "exposure must be counted exactly once")
	assert.Len(t, listEvents(t, srv, "C-2").Events, 2)
}

func TestConcurrentCreateSameBarcode(t *testing.T) {
	srv := newServer(t)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{
				"barcode": "C-3", "allowedSeconds": 10, "createdAt": "2026-09-13T08:00:00Z",
			})
			res, err := http.Post(srv.URL+"/api/batches", "application/json", bytes.NewReader(raw))
			if err != nil {
				codes[i] = -1
				return
			}
			res.Body.Close()
			codes[i] = res.StatusCode
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, wins)
}

// --- misc -------------------------------------------------------------------

func TestHealth(t *testing.T) {
	srv := newServer(t)
	code, raw := doJSON(t, http.MethodGet, srv.URL+"/api/health", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"ok": true}`, string(raw))
}

func TestEventOnUnknownBatch(t *testing.T) {
	srv := newServer(t)
	code, _, e := postEvent(t, srv, "GHOST", "takeout", "2026-09-13T08:00:10Z")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", e.Error.Code)
}

// --- revocation ---------------------------------------------------------------

func revokeEvent(t *testing.T, srv *httptest.Server, barcode string, eventID int64, at, reason string) (int, batchResp, errResp) {
	t.Helper()
	code, raw := doJSON(t, http.MethodPost,
		srv.URL+"/api/batches/"+barcode+"/events/"+strconv.FormatInt(eventID, 10)+"/revocation",
		map[string]string{"at": at, "reason": reason})
	var b batchResp
	var e errResp
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &b))
	} else {
		require.NoError(t, json.Unmarshal(raw, &e))
	}
	return code, b, e
}

// revokeRaw is a goroutine-safe variant returning (status, errorCode).
func revokeRaw(srv *httptest.Server, barcode string, eventID int64, at, reason string) (int, string) {
	raw, err := json.Marshal(map[string]string{"at": at, "reason": reason})
	if err != nil {
		return -1, err.Error()
	}
	res, err := http.Post(srv.URL+"/api/batches/"+barcode+"/events/"+
		strconv.FormatInt(eventID, 10)+"/revocation", "application/json", bytes.NewReader(raw))
	if err != nil {
		return -1, err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		var e errResp
		if json.Unmarshal(body, &e) == nil {
			return res.StatusCode, e.Error.Code
		}
	}
	return res.StatusCode, ""
}

// Main acceptance flow: a mistaken return scraps the batch; revoking it
// restores the out-of-cabinet state and the previous accumulated total, after
// which a correct return works again.
func TestRevokeMistakenReturnRestoresOutAndAccumulated(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-1", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-1", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "R-1", "return", "2026-09-13T08:00:30Z") // legitimate +20
	postEvent(t, srv, "R-1", "takeout", "2026-09-13T08:00:40Z")

	// Mistaken return: +90 would push accumulated to 110 and scrap the batch.
	code, b, _ := postEvent(t, srv, "R-1", "return", "2026-09-13T08:02:10Z")
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, int64(110), b.AccumulatedSeconds)
	require.Equal(t, "scrapped", b.Status)
	require.Equal(t, "in", b.State)

	// Revoke the mistaken return (event id 4) at a later time with a reason.
	code, b, _ = revokeEvent(t, srv, "R-1", 4, "2026-09-13T08:03:00Z", "误扫归还，样本仍在柜外")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "out", b.State, "revoking a return restores out-of-cabinet state")
	assert.Equal(t, "usable", b.Status, "restored accumulated 20 <= limit 100")
	assert.True(t, b.Usable)
	assert.Equal(t, int64(20), b.AccumulatedSeconds, "the revoked exposure is clawed back")
	assert.Equal(t, int64(80), b.RemainingSeconds)
	require.NotNil(t, b.LastEvent, "lastEvent reverts to the matching takeout")
	assert.Equal(t, "takeout", b.LastEvent.Type)
	assert.Equal(t, "2026-09-13T08:00:40Z", b.LastEvent.At)

	// Ledger: 4 rows; the takeout (id 3) stays active, the mistaken return
	// (id 4) is annotated with its revocation audit fields.
	evs := listEvents(t, srv, "R-1")
	require.Len(t, evs.Events, 4)
	assert.Equal(t, int64(3), evs.Events[2].ID)
	assert.Nil(t, evs.Events[2].RevokedAt, "the open takeout stays active")
	revoked := evs.Events[3]
	assert.Equal(t, int64(4), revoked.ID)
	assert.Equal(t, "return", revoked.Type)
	require.NotNil(t, revoked.RevokedAt)
	assert.Equal(t, "2026-09-13T08:03:00Z", *revoked.RevokedAt)
	require.NotNil(t, revoked.RevokeReason)
	assert.Equal(t, "误扫归还，样本仍在柜外", *revoked.RevokeReason)

	// The batch can now be returned correctly (timestamp later than the
	// revocation). Exposure is recomputed from the reopened takeout, so this
	// genuinely-long trip legitimately scraps again — but it is accepted as a
	// normal return and counted exactly once, proving the machine recovered.
	code, b, _ = postEvent(t, srv, "R-1", "return", "2026-09-13T08:03:10Z")
	require.Equal(t, http.StatusCreated, code, "a correct return after undo works again")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(170), b.AccumulatedSeconds, "20 (trip 1) + 150 (08:00:40 -> 08:03:10)")
	assert.Len(t, listEvents(t, srv, "R-1").Events, 5, "new events append; revoked rows are retained")
}

// Undoing a mistaken (but not yet over-limit) return lets the operator redo it
// with the real return time, ending usable within the budget.
func TestRevokeMistakenReturnThenCorrectReturnStaysUsable(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-1b", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-1b", "takeout", "2026-09-13T08:00:05Z")
	postEvent(t, srv, "R-1b", "return", "2026-09-13T08:00:35Z") // mistyped: +30
	b := getBatch(t, srv, "R-1b")
	require.Equal(t, int64(30), b.AccumulatedSeconds)
	require.Equal(t, "in", b.State)

	code, rb, _ := revokeEvent(t, srv, "R-1b", 2, "2026-09-13T08:00:40Z", "归还时刻扫错")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "out", rb.State)
	assert.Equal(t, int64(0), rb.AccumulatedSeconds)
	assert.Equal(t, "usable", rb.Status)

	code, rb, _ = postEvent(t, srv, "R-1b", "return", "2026-09-13T08:00:50Z") // real +45
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "in", rb.State)
	assert.Equal(t, int64(45), rb.AccumulatedSeconds)
	assert.Equal(t, "usable", rb.Status)
}

func TestRevokeTakeoutBackInside(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-2", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-2", "takeout", "2026-09-13T08:00:10Z")
	b := getBatch(t, srv, "R-2")
	require.Equal(t, "out", b.State)

	code, rb, _ := revokeEvent(t, srv, "R-2", 1, "2026-09-13T08:00:20Z", "误扫取出")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "in", rb.State, "revoking a takeout moves the batch back inside")
	assert.Equal(t, int64(0), rb.AccumulatedSeconds)
	assert.Nil(t, rb.LastEvent, "no active event remains")
	assert.Len(t, listEvents(t, srv, "R-2").Events, 1, "revoked row retained in ledger")

	// Operations can continue correctly from the restored in-cabinet state.
	code, rb, _ = postEvent(t, srv, "R-2", "takeout", "2026-09-13T08:00:30Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "out", rb.State)
}

func TestRevokeTakeoutRevertsToPreviousReturn(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-2b", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-2b", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "R-2b", "return", "2026-09-13T08:00:20Z") // +10
	postEvent(t, srv, "R-2b", "takeout", "2026-09-13T08:00:30Z")

	code, rb, _ := revokeEvent(t, srv, "R-2b", 3, "2026-09-13T08:00:40Z", "误扫取出")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "in", rb.State)
	assert.Equal(t, int64(10), rb.AccumulatedSeconds)
	require.NotNil(t, rb.LastEvent)
	assert.Equal(t, "return", rb.LastEvent.Type, "lastEvent reverts to the previous return (id 2)")
	assert.Equal(t, "2026-09-13T08:00:20Z", rb.LastEvent.At)
}

func TestRevokeNonLastEventRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-3", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-3", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "R-3", "return", "2026-09-13T08:00:20Z")

	// Event 1 (takeout) is not the last active event (2 is).
	code, _, e := revokeEvent(t, srv, "R-3", 1, "2026-09-13T08:00:30Z", "尝试撤销旧记录")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "not_last_event", e.Error.Code)

	b := getBatch(t, srv, "R-3")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(10), b.AccumulatedSeconds, "failed revoke changes nothing")
	evs := listEvents(t, srv, "R-3")
	require.Len(t, evs.Events, 2)
	assert.Nil(t, evs.Events[0].RevokedAt)
	assert.Nil(t, evs.Events[1].RevokedAt)
}

func TestDuplicateRevocationRejected(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-4", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-4", "takeout", "2026-09-13T08:00:10Z")
	code, _, _ := revokeEvent(t, srv, "R-4", 1, "2026-09-13T08:00:20Z", "第一次撤销")
	require.Equal(t, http.StatusOK, code)

	// Same event again: already revoked → 409.
	code, _, e := revokeEvent(t, srv, "R-4", 1, "2026-09-13T08:00:30Z", "重复撤销")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "already_revoked", e.Error.Code)

	// The row stays revoked: it is rejected as an already-revoked target.
	code, _, e = revokeEvent(t, srv, "R-4", 1, "2026-09-13T08:00:40Z", "再试")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "already_revoked", e.Error.Code)

	b := getBatch(t, srv, "R-4")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
}

func TestRevokeThenRevokePreviousEvent(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-4b", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-4b", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "R-4b", "return", "2026-09-13T08:00:20Z")
	postEvent(t, srv, "R-4b", "takeout", "2026-09-13T08:00:30Z")

	// Undo in order: newest first.
	code, _, _ := revokeEvent(t, srv, "R-4b", 3, "2026-09-13T08:00:40Z", "撤销误取出")
	require.Equal(t, http.StatusOK, code)
	code, b, _ := revokeEvent(t, srv, "R-4b", 2, "2026-09-13T08:00:50Z", "撤销误归还")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "out", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	require.NotNil(t, b.LastEvent)
	assert.Equal(t, "takeout", b.LastEvent.Type)
	assert.Equal(t, "2026-09-13T08:00:10Z", b.LastEvent.At)
}

func TestRevokeTimeValidation(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-5", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-5", "takeout", "2026-09-13T08:00:10Z")

	// Revocation not strictly later than the last operation (the takeout).
	code, _, e := revokeEvent(t, srv, "R-5", 1, "2026-09-13T08:00:10Z", "同时刻")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	code, _, e = revokeEvent(t, srv, "R-5", 1, "2026-09-13T08:00:05Z", "更早")
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "time_not_monotonic", e.Error.Code)

	// Bad time format.
	code, _, e = revokeEvent(t, srv, "R-5", 1, "2026-09-13T08:00:20", "缺Z")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_time", e.Error.Code)

	// Empty/whitespace reason.
	code, _, e = revokeEvent(t, srv, "R-5", 1, "2026-09-13T08:00:20Z", "   ")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "reason_required", e.Error.Code)

	b := getBatch(t, srv, "R-5")
	assert.Equal(t, "out", b.State)
	assert.Nil(t, b.LastEvent.RevokedAt)
}

func TestRevokeUnknownBatchAndEvent(t *testing.T) {
	srv := newServer(t)
	code, _, e := revokeEvent(t, srv, "GHOST", 1, "2026-09-13T08:00:20Z", "x")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", e.Error.Code)

	createBatch(t, srv, "R-6", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-6", "takeout", "2026-09-13T08:00:10Z")

	// Bad path id.
	code, raw := doJSON(t, http.MethodPost, srv.URL+"/api/batches/R-6/events/abc/revocation",
		map[string]string{"at": "2026-09-13T08:00:20Z", "reason": "x"})
	assert.Equal(t, http.StatusBadRequest, code)
	require.NoError(t, json.Unmarshal(raw, &e))
	assert.Equal(t, "invalid_event_id", e.Error.Code)

	// Non-existent event id.
	code, _, e = revokeEvent(t, srv, "R-6", 999, "2026-09-13T08:00:20Z", "x")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "not_found", e.Error.Code)
}

func TestRevokeIsTransactionalOnFailure(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-7", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-7", "takeout", "2026-09-13T08:00:10Z")
	postEvent(t, srv, "R-7", "return", "2026-09-13T08:00:20Z")
	pre := getBatch(t, srv, "R-7")

	// A rejected revoke (non-last event) must leave the aggregate exactly as
	// it was and add no audit annotation.
	code, _, _ := revokeEvent(t, srv, "R-7", 1, "2026-09-13T08:00:30Z", "拒绝的撤销")
	require.Equal(t, http.StatusConflict, code)
	post := getBatch(t, srv, "R-7")
	assert.Equal(t, pre.State, post.State)
	assert.Equal(t, pre.AccumulatedSeconds, post.AccumulatedSeconds)
	assert.Equal(t, pre.Status, post.Status)
	for _, ev := range listEvents(t, srv, "R-7").Events {
		assert.Nil(t, ev.RevokedAt)
	}
}

func TestConcurrentRevokeOnlyOneWins(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-8", 100, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-8", "takeout", "2026-09-13T08:00:10Z")

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	errCodes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], errCodes[i] = revokeRaw(srv, "R-8", 1, "2026-09-13T08:00:20Z", "并发撤销")
		}(i)
	}
	wg.Wait()

	wins := 0
	for i, c := range codes {
		if c == http.StatusOK {
			wins++
		} else {
			assert.Equal(t, http.StatusConflict, c, "i=%d code=%s", i, errCodes[i])
		}
	}
	assert.Equal(t, 1, wins, "two stations revoking concurrently: exactly one may commit")

	b := getBatch(t, srv, "R-8")
	assert.Equal(t, "in", b.State)
	assert.Equal(t, int64(0), b.AccumulatedSeconds)
	evs := listEvents(t, srv, "R-8")
	require.Len(t, evs.Events, 1)
	require.NotNil(t, evs.Events[0].RevokedAt, "audit field persisted exactly once")
}

// Existing critical timing flows must keep their exact results when the
// revocation capability is never used (regression guard for the added audit
// columns / adjusted queries).
func TestNoRevocationCriticalFlowsUnchanged(t *testing.T) {
	srv := newServer(t)
	createBatch(t, srv, "R-9", 10, "2026-09-13T08:00:00Z")
	postEvent(t, srv, "R-9", "takeout", "2026-09-13T08:00:05Z")
	code, b, _ := postEvent(t, srv, "R-9", "return", "2026-09-13T08:00:15Z")
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, int64(10), b.AccumulatedSeconds)
	assert.Equal(t, "usable", b.Status)
	require.NotNil(t, b.LastEvent)
	assert.Nil(t, b.LastEvent.RevokedAt)
	assert.Nil(t, b.LastEvent.RevokeReason)
	for _, ev := range listEvents(t, srv, "R-9").Events {
		assert.Nil(t, ev.RevokedAt)
		assert.Nil(t, ev.RevokeReason)
	}
}
