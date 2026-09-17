package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		ID           int64  `json:"id"`
		Type         string `json:"type"`
		At           string `json:"at"`
		DeltaSeconds *int64 `json:"deltaSeconds"`
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
		ID           int64  `json:"id"`
		Type         string `json:"type"`
		At           string `json:"at"`
		DeltaSeconds *int64 `json:"deltaSeconds"`
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
