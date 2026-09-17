// Command verify runs the one-shot acceptance suite against a deployed
// timing-station stack (API + web). It exits 0 when every check passes,
// 1 otherwise. Configure targets with API_BASE / WEB_BASE.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	apiBase = envOr("API_BASE", "http://api:8080/api")
	webBase = envOr("WEB_BASE", "http://web:80")

	client = &http.Client{Timeout: 10 * time.Second}

	failures int
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func check(name string, ok bool, detail ...string) {
	if ok {
		fmt.Printf("PASS  %s\n", name)
		return
	}
	failures++
	fmt.Printf("FAIL  %s  %s\n", name, strings.Join(detail, " "))
}

type batch struct {
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
		UndoneAt     *string `json:"undoneAt"`
		UndoReason   *string `json:"undoReason"`
	} `json:"lastEvent"`
}

type eventRow struct {
	ID           int64   `json:"id"`
	Type         string  `json:"type"`
	At           string  `json:"at"`
	DeltaSeconds *int64  `json:"deltaSeconds"`
	UndoneAt     *string `json:"undoneAt"`
	UndoReason   *string `json:"undoReason"`
}

type apiErr struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func do(method, url string, body any) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return -1, []byte(err.Error())
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func createBatch(barcode string, allowed int64, createdAt string) (int, batch, apiErr) {
	code, raw := do("POST", apiBase+"/batches", map[string]any{
		"barcode": barcode, "allowedSeconds": allowed, "createdAt": createdAt,
	})
	return decodeBatch(code, raw)
}

func postEvent(barcode, typ, at string) (int, batch, apiErr) {
	code, raw := do("POST", apiBase+"/batches/"+barcode+"/events", map[string]string{
		"type": typ, "at": at,
	})
	return decodeBatch(code, raw)
}

func undoEvent(barcode string, id int64, at, reason string) (int, batch, apiErr) {
	code, raw := do("POST", apiBase+"/batches/"+barcode+"/events/"+strconv.FormatInt(id, 10)+"/undo",
		map[string]string{"at": at, "reason": reason})
	return decodeBatch(code, raw)
}

func listEvents(barcode string) []eventRow {
	code, raw := do("GET", apiBase+"/batches/"+barcode+"/events", nil)
	if code != 200 {
		return nil
	}
	var res struct {
		Events []eventRow `json:"events"`
	}
	_ = json.Unmarshal(raw, &res)
	return res.Events
}

func decodeBatch(code int, raw []byte) (int, batch, apiErr) {
	var b batch
	var e apiErr
	if code == 200 || code == 201 {
		_ = json.Unmarshal(raw, &b)
	} else {
		_ = json.Unmarshal(raw, &e)
	}
	return code, b, e
}

func getBatch(barcode string) (int, batch) {
	code, raw := do("GET", apiBase+"/batches/"+barcode, nil)
	var b batch
	_ = json.Unmarshal(raw, &b)
	return code, b
}

func eventCount(barcode string) int {
	code, raw := do("GET", apiBase+"/batches/"+barcode+"/events", nil)
	if code != 200 {
		return -1
	}
	var res struct {
		Events []json.RawMessage `json:"events"`
	}
	_ = json.Unmarshal(raw, &res)
	return len(res.Events)
}

func waitForAPI() {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := do("GET", apiBase+"/health", nil); code == 200 {
			return
		}
		time.Sleep(time.Second)
	}
	fmt.Println("FAIL  api did not become healthy within 60s")
	os.Exit(1)
}

func waitForWeb() (int, []byte) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, raw := do("GET", webBase+"/", nil)
		if code == 200 {
			return code, raw
		}
		if time.Now().After(deadline) {
			return code, raw
		}
		time.Sleep(time.Second)
	}
}

func main() {
	fmt.Printf("verify: API_BASE=%s WEB_BASE=%s\n", apiBase, webBase)
	waitForAPI()

	// 0. Web UI is served.
	code, raw := waitForWeb()
	check("web serves the scan-station page", code == 200 && strings.Contains(string(raw), "id=\"app\""),
		fmt.Sprintf("status=%d", code))

	uniq := time.Now().UnixNano()

	// 1. Lifecycle with boundary return: accumulated == limit stays usable.
	b1 := fmt.Sprintf("VERIFY-A-%d", uniq)
	code, b, _ := createBatch(b1, 10, "2026-01-01T00:00:00Z")
	check("create batch (allowed=10s)", code == 201 && b.State == "in" && b.AccumulatedSeconds == 0 && b.Status == "usable",
		fmt.Sprintf("status=%d state=%s", code, b.State))

	code, _, e := createBatch(b1, 10, "2026-01-01T00:00:00Z")
	check("duplicate barcode -> 409", code == 409 && e.Error.Code == "duplicate_barcode", fmt.Sprintf("status=%d", code))

	code, _, e = postEvent(b1, "takeout", "2026-01-01T00:00:00Z")
	check("event time equal to previous -> 409", code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = postEvent(b1, "return", "2026-01-01T00:00:05Z")
	check("return without takeout -> 409", code == 409 && e.Error.Code == "invalid_transition", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, b, _ = postEvent(b1, "takeout", "2026-01-01T00:00:05Z")
	check("takeout succeeds", code == 201 && b.State == "out", fmt.Sprintf("status=%d state=%s", code, b.State))

	code, _, e = postEvent(b1, "takeout", "2026-01-01T00:00:06Z")
	check("consecutive takeout -> 409", code == 409 && e.Error.Code == "invalid_transition", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = postEvent(b1, "return", "2026-01-01T00:00:04Z")
	check("out-of-order return -> 409", code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, b, _ = postEvent(b1, "return", "2026-01-01T00:00:15Z")
	check("boundary return (== limit) stays usable",
		code == 201 && b.AccumulatedSeconds == 10 && b.RemainingSeconds == 0 && b.Status == "usable" && b.Usable,
		fmt.Sprintf("status=%d acc=%d batchStatus=%s", code, b.AccumulatedSeconds, b.Status))

	code, _, e = postEvent(b1, "return", "2026-01-01T00:00:16Z")
	check("duplicate return -> 409", code == 409 && e.Error.Code == "invalid_transition", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	_, b = getBatch(b1)
	check("duplicate return did not count twice", b.AccumulatedSeconds == 10, fmt.Sprintf("acc=%d", b.AccumulatedSeconds))
	check("rejected events wrote nothing", eventCount(b1) == 2, fmt.Sprintf("events=%d", eventCount(b1)))

	// 2. Over-limit return scraps immediately and permanently.
	code, b, _ = postEvent(b1, "takeout", "2026-01-01T00:00:20Z")
	check("takeout at boundary still allowed", code == 201, fmt.Sprintf("status=%d", code))
	code, b, _ = postEvent(b1, "return", "2026-01-01T00:00:21Z")
	check("over-limit return scraps immediately",
		code == 201 && b.AccumulatedSeconds == 11 && b.Status == "scrapped" && !b.Usable,
		fmt.Sprintf("status=%d acc=%d batchStatus=%s", code, b.AccumulatedSeconds, b.Status))
	code, _, e = postEvent(b1, "takeout", "2026-01-01T00:00:30Z")
	check("scrapped batch cannot be taken out -> 409", code == 409 && e.Error.Code == "batch_scrapped", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	// 3. Concurrency: the same old state succeeds at most once.
	b2 := fmt.Sprintf("VERIFY-B-%d", uniq)
	createBatch(b2, 1000, "2026-01-01T00:00:00Z")
	const racers = 8
	codes := make([]int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, _ := postEvent(b2, "takeout", "2026-01-01T00:00:05Z")
			codes[i] = c
		}(i)
	}
	wg.Wait()
	check("racing takeouts: exactly one wins", countCode(codes, 201) == 1 && countCode(codes, 409) == racers-1,
		fmt.Sprintf("codes=%v", codes))

	for i := range codes {
		codes[i] = 0
	}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, _ := postEvent(b2, "return", "2026-01-01T00:00:15Z")
			codes[i] = c
		}(i)
	}
	wg.Wait()
	check("racing returns: exactly one wins", countCode(codes, 201) == 1 && countCode(codes, 409) == racers-1,
		fmt.Sprintf("codes=%v", codes))
	_, b = getBatch(b2)
	check("racing returns counted exposure exactly once", b.AccumulatedSeconds == 10 && b.State == "in",
		fmt.Sprintf("acc=%d state=%s", b.AccumulatedSeconds, b.State))
	check("race wrote exactly two events", eventCount(b2) == 2, fmt.Sprintf("events=%d", eventCount(b2)))

	// 4. State is persisted and re-readable (what the page shows after refresh).
	_, b = getBatch(b1)
	check("final state of scrapped batch is re-readable",
		b.State == "in" && b.Status == "scrapped" && b.AccumulatedSeconds == 11 && b.LastEvent != nil && b.LastEvent.Type == "return",
		fmt.Sprintf("state=%s status=%s acc=%d", b.State, b.Status, b.AccumulatedSeconds))

	// 5. Main undo flow: a mis-scanned return scraps the batch; undoing it
	// restores the out-of-cabinet state with the original accumulated total.
	b3 := fmt.Sprintf("VERIFY-C-%d", uniq)
	createBatch(b3, 10, "2026-01-01T00:00:00Z")
	postEvent(b3, "takeout", "2026-01-01T00:00:05Z")
	code, b, _ = postEvent(b3, "return", "2026-01-01T00:00:16Z") // +11 > 10
	check("mis-scanned over-limit return scraps the batch",
		code == 201 && b.Status == "scrapped" && b.AccumulatedSeconds == 11,
		fmt.Sprintf("status=%d batchStatus=%s acc=%d", code, b.Status, b.AccumulatedSeconds))

	evs := listEvents(b3)
	returnID := evs[1].ID
	code, b, _ = undoEvent(b3, returnID, "2026-01-01T00:00:20Z", "误扫归还")
	check("undo mistaken return restores out-of-cabinet + original accumulated",
		code == 200 && b.State == "out" && b.AccumulatedSeconds == 0 && b.Status == "usable" && b.Usable,
		fmt.Sprintf("status=%d state=%s acc=%d batchStatus=%s", code, b.State, b.AccumulatedSeconds, b.Status))

	evs = listEvents(b3)
	check("undone event keeps undo time and reason in the audit trail",
		len(evs) == 2 && evs[0].UndoneAt == nil &&
			evs[1].UndoneAt != nil && *evs[1].UndoneAt == "2026-01-01T00:00:20Z" &&
			evs[1].UndoReason != nil && *evs[1].UndoReason == "误扫归还",
		fmt.Sprintf("events=%v", evs))
	_, b = getBatch(b3)
	check("lastEvent falls back to the still-active takeout",
		b.LastEvent != nil && b.LastEvent.Type == "takeout" && b.LastEvent.At == "2026-01-01T00:00:05Z",
		fmt.Sprintf("lastEvent=%+v", b.LastEvent))

	code, b, _ = postEvent(b3, "return", "2026-01-01T00:00:14Z")
	check("batch can be returned correctly after the undo",
		code == 201 && b.State == "in" && b.AccumulatedSeconds == 9 && b.Status == "usable",
		fmt.Sprintf("status=%d state=%s acc=%d batchStatus=%s", code, b.State, b.AccumulatedSeconds, b.Status))

	// 6. Undo of a mis-scanned takeout puts the batch back into the cabinet.
	b4 := fmt.Sprintf("VERIFY-D-%d", uniq)
	createBatch(b4, 100, "2026-01-01T00:00:00Z")
	postEvent(b4, "takeout", "2026-01-01T00:00:05Z")
	evs = listEvents(b4)
	code, b, _ = undoEvent(b4, evs[0].ID, "2026-01-01T00:00:06Z", "误扫取出")
	check("undo takeout returns the batch to the cabinet",
		code == 200 && b.State == "in" && b.AccumulatedSeconds == 0 && b.LastEvent == nil,
		fmt.Sprintf("status=%d state=%s acc=%d", code, b.State, b.AccumulatedSeconds))
	code, b, _ = postEvent(b4, "takeout", "2026-01-01T00:00:07Z")
	check("takeout works again after the undo", code == 201 && b.State == "out",
		fmt.Sprintf("status=%d state=%s", code, b.State))

	// 7. Undo rejections are clear conflicts and write nothing.
	b5 := fmt.Sprintf("VERIFY-E-%d", uniq)
	createBatch(b5, 100, "2026-01-01T00:00:00Z")
	postEvent(b5, "takeout", "2026-01-01T00:00:05Z")
	postEvent(b5, "return", "2026-01-01T00:00:10Z") // +5
	evs = listEvents(b5)

	code, _, e = undoEvent(b5, evs[0].ID, "2026-01-01T00:00:11Z", "非最近记录")
	check("undoing a non-last event -> 409", code == 409 && e.Error.Code == "not_last_event",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, b, _ = undoEvent(b5, evs[1].ID, "2026-01-01T00:00:11Z", "误扫归还")
	check("undo last return succeeds", code == 200 && b.State == "out" && b.AccumulatedSeconds == 0,
		fmt.Sprintf("status=%d state=%s acc=%d", code, b.State, b.AccumulatedSeconds))

	code, _, e = undoEvent(b5, evs[1].ID, "2026-01-01T00:00:12Z", "重复撤销")
	check("duplicate undo -> 409", code == 409 && e.Error.Code == "already_undone",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = undoEvent(b5, evs[0].ID, "2026-01-01T00:00:04Z", "时刻早于最后操作")
	check("undo time earlier than last operation -> 409", code == 409 && e.Error.Code == "time_not_monotonic",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = undoEvent(b5, evs[0].ID, "2026-01-01T00:00:12Z", "   ")
	check("empty undo reason -> 400", code == 400 && e.Error.Code == "reason_required",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = undoEvent(b5, evs[0].ID, "2026-01-01 00:00:12", "误扫")
	check("malformed undo time -> 400", code == 400 && e.Error.Code == "invalid_time",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = undoEvent(b5, 999999, "2026-01-01T00:00:12Z", "误扫")
	check("undo unknown event -> 404", code == 404 && e.Error.Code == "not_found",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	code, _, e = undoEvent("VERIFY-GHOST", 1, "2026-01-01T00:00:12Z", "误扫")
	check("undo on unknown batch -> 404", code == 404 && e.Error.Code == "not_found",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	_, b = getBatch(b5)
	check("rejected undos changed nothing",
		b.State == "out" && b.AccumulatedSeconds == 0 && eventCount(b5) == 2,
		fmt.Sprintf("state=%s acc=%d events=%d", b.State, b.AccumulatedSeconds, eventCount(b5)))

	// 8. Two stations undoing the same event concurrently: exactly one wins.
	b6 := fmt.Sprintf("VERIFY-F-%d", uniq)
	createBatch(b6, 100, "2026-01-01T00:00:00Z")
	postEvent(b6, "takeout", "2026-01-01T00:00:05Z")
	postEvent(b6, "return", "2026-01-01T00:00:10Z") // +5
	evs = listEvents(b6)
	returnID = evs[1].ID
	for i := range codes {
		codes[i] = 0
	}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, _ := undoEvent(b6, returnID, "2026-01-01T00:00:11Z", "误扫归还")
			codes[i] = c
		}(i)
	}
	wg.Wait()
	check("racing undos: exactly one wins", countCode(codes, 200) == 1 && countCode(codes, 409) == racers-1,
		fmt.Sprintf("codes=%v", codes))
	_, b = getBatch(b6)
	check("racing undos restored the exposure exactly once",
		b.State == "out" && b.AccumulatedSeconds == 0,
		fmt.Sprintf("state=%s acc=%d", b.State, b.AccumulatedSeconds))
	evs = listEvents(b6)
	check("race left exactly one undo audit",
		len(evs) == 2 && evs[0].UndoneAt == nil && evs[1].UndoneAt != nil && *evs[1].UndoneAt == "2026-01-01T00:00:11Z",
		fmt.Sprintf("events=%v", evs))

	if failures > 0 {
		fmt.Printf("\nverify: %d check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nverify: all checks passed")
}

func countCode(codes []int, want int) int {
	n := 0
	for _, c := range codes {
		if c == want {
			n++
		}
	}
	return n
}
