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

// requiref records a failure and aborts the run: subsequent checks depend on
// this precondition and would only produce noise without it.
func requiref(name string, ok bool, detail ...string) {
	if !ok {
		check(name, false, detail...)
		fmt.Printf("\nverify: precondition failed, aborting\n")
		os.Exit(1)
	}
	fmt.Printf("PASS  %s\n", name)
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
		RevokedAt    *string `json:"revokedAt"`
		RevokeReason *string `json:"revokeReason"`
	} `json:"lastEvent"`
}

type event struct {
	ID           int64   `json:"id"`
	Type         string  `json:"type"`
	At           string  `json:"at"`
	DeltaSeconds *int64  `json:"deltaSeconds"`
	RevokedAt    *string `json:"revokedAt"`
	RevokeReason *string `json:"revokeReason"`
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
	return len(listEvents(barcode))
}

func listEvents(barcode string) []event {
	code, raw := do("GET", apiBase+"/batches/"+barcode+"/events", nil)
	if code != 200 {
		return nil
	}
	var res struct {
		Events []event `json:"events"`
	}
	_ = json.Unmarshal(raw, &res)
	return res.Events
}

// revokeEvent posts a revocation for a specific event id.
func revokeEvent(barcode string, id int64, at, reason string) (int, batch, apiErr) {
	code, raw := do("POST",
		apiBase+"/batches/"+barcode+"/events/"+strconv.FormatInt(id, 10)+"/revocation",
		map[string]string{"at": at, "reason": reason})
	return decodeBatch(code, raw)
}

// revokeRaw is goroutine-safe, returning (status, errorCode).
func revokeRaw(barcode string, id int64, at, reason string) (int, string) {
	raw, _ := json.Marshal(map[string]string{"at": at, "reason": reason})
	req, _ := http.NewRequest("POST",
		apiBase+"/batches/"+barcode+"/events/"+strconv.FormatInt(id, 10)+"/revocation",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return -1, err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		var e apiErr
		if json.Unmarshal(body, &e) == nil {
			return res.StatusCode, e.Error.Code
		}
	}
	return res.StatusCode, ""
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

	// 5. Revocation — main flow: mistaken return scraps; undo restores the
	// out-of-cabinet state and previous accumulated total; a correct return
	// afterwards works again.
	br := fmt.Sprintf("VERIFY-R-%d", uniq)
	code, b, _ = createBatch(br, 100, "2026-02-01T00:00:00Z")
	check("revoke: create batch", code == 201, fmt.Sprintf("status=%d", code))
	code, _, _ = postEvent(br, "takeout", "2026-02-01T00:00:10Z")
	check("revoke: takeout", code == 201, fmt.Sprintf("status=%d", code))
	code, b, _ = postEvent(br, "return", "2026-02-01T00:02:30Z") // +140 > 100 → scrapped
	check("revoke: mistaken return scraps batch",
		code == 201 && b.AccumulatedSeconds == 140 && b.Status == "scrapped" && b.State == "in",
		fmt.Sprintf("status=%d acc=%d status=%s", code, b.AccumulatedSeconds, b.Status))

	// Revoke the mistaken return with a later Z time and a reason. Event ids
	// are globally auto-incremented across batches, so read them from ledger.
	rEv := listEvents(br)
	requiref("revoke setup: two ledger events", len(rEv) == 2, fmt.Sprintf("events=%d", len(rEv)))
	returnID := rEv[1].ID
	code, b, e = revokeEvent(br, returnID, "2026-02-01T00:03:00Z", "误扫归还，样本实际仍在柜外")
	check("revoke mistaken return restores out + usable + original accumulated",
		code == 200 && b.State == "out" && b.Status == "usable" && b.Usable && b.AccumulatedSeconds == 0 && b.RemainingSeconds == 100,
		fmt.Sprintf("status=%d state=%s status=%s acc=%d", code, b.State, b.Status, b.AccumulatedSeconds))
	check("revoke restores lastEvent to matching takeout",
		b.LastEvent != nil && b.LastEvent.Type == "takeout" && b.LastEvent.At == "2026-02-01T00:00:10Z",
		fmt.Sprintf("lastEvent=%v", b.LastEvent))

	// Audit fields are attached to the revoked event in the ledger.
	revokedEvs := listEvents(br)
	auditOK := len(revokedEvs) == 2 && revokedEvs[1].ID == returnID &&
		revokedEvs[1].RevokedAt != nil && *revokedEvs[1].RevokedAt == "2026-02-01T00:03:00Z" &&
		revokedEvs[1].RevokeReason != nil && *revokedEvs[1].RevokeReason == "误扫归还，样本实际仍在柜外" &&
		revokedEvs[0].RevokedAt == nil
	check("revoke writes audit time+reason on event and keeps ledger rows", auditOK,
		fmt.Sprintf("events=%d", len(revokedEvs)))

	// A correct return after undo is accepted again. Exposure is recomputed
	// from the reopened takeout: 00:00:10 -> 00:03:10 = 180s, so it scraps —
	// the point is the machine is operable again and counts the real trip
	// once, not that this particular late return fits the budget.
	code, b, _ = postEvent(br, "return", "2026-02-01T00:03:10Z")
	check("revoke: a further correct return is accepted and counted once",
		code == 201 && b.State == "in" && b.AccumulatedSeconds == 180,
		fmt.Sprintf("status=%d acc=%d", code, b.AccumulatedSeconds))

	// 5b. A mistaken (but not over-limit) return: undo restores out/原累计,
	// then the real return is recorded and the batch stays usable.
	bq := fmt.Sprintf("VERIFY-Q-%d", uniq)
	createBatch(bq, 600, "2026-02-02T00:00:00Z")
	postEvent(bq, "takeout", "2026-02-02T00:00:10Z")
	code, b, _ = postEvent(bq, "return", "2026-02-02T00:00:40Z") // stray scan: +30
	check("revoke: stray (usable) return recorded", code == 201 && b.AccumulatedSeconds == 30 && b.Status == "usable",
		fmt.Sprintf("acc=%d", b.AccumulatedSeconds))
	qEv := listEvents(bq)
	requiref("revoke setup: two ledger events", len(qEv) == 2, fmt.Sprintf("events=%d", len(qEv)))
	code, b, _ = revokeEvent(bq, qEv[1].ID, "2026-02-02T00:00:50Z", "误扫归还，样本仍在柜外")
	check("revoke usable mistaken return restores out and original accumulated",
		code == 200 && b.State == "out" && b.AccumulatedSeconds == 0 && b.Status == "usable",
		fmt.Sprintf("state=%s acc=%d status=%s", b.State, b.AccumulatedSeconds, b.Status))
	code, b, _ = postEvent(bq, "return", "2026-02-02T00:01:00Z") // real +50
	check("revoke: correct return afterwards accepted and stays usable",
		code == 201 && b.State == "in" && b.AccumulatedSeconds == 50 && b.Status == "usable",
		fmt.Sprintf("acc=%d status=%s", b.AccumulatedSeconds, b.Status))

	// 6. Revocation — mistaken takeout: batch goes back inside.
	bt := fmt.Sprintf("VERIFY-T-%d", uniq)
	createBatch(bt, 100, "2026-03-01T00:00:00Z")
	postEvent(bt, "takeout", "2026-03-01T00:00:10Z")
	tEv := listEvents(bt)
	requiref("revoke setup: one takeout event", len(tEv) == 1, fmt.Sprintf("events=%d", len(tEv)))
	code, b, _ = revokeEvent(bt, tEv[0].ID, "2026-03-01T00:00:20Z", "误扫取出")
	check("revoke mistaken takeout restores in-cabinet",
		code == 200 && b.State == "in" && b.AccumulatedSeconds == 0 && b.LastEvent == nil,
		fmt.Sprintf("status=%d state=%s acc=%d last=%v", code, b.State, b.AccumulatedSeconds, b.LastEvent))
	code, b, _ = postEvent(bt, "takeout", "2026-03-01T00:00:30Z")
	check("revoke: operations continue from restored state",
		code == 201 && b.State == "out", fmt.Sprintf("status=%d state=%s", code, b.State))

	// 7. Revoking a non-last (or already revoked) event is rejected.
	bn := fmt.Sprintf("VERIFY-N-%d", uniq)
	createBatch(bn, 100, "2026-04-01T00:00:00Z")
	postEvent(bn, "takeout", "2026-04-01T00:00:10Z")
	postEvent(bn, "return", "2026-04-01T00:00:20Z")
	nEv := listEvents(bn)
	requiref("revoke setup: takeout+return events", len(nEv) == 2, fmt.Sprintf("events=%d", len(nEv)))
	nTakeoutID, nReturnID := nEv[0].ID, nEv[1].ID
	code, _, e = revokeEvent(bn, nTakeoutID, "2026-04-01T00:00:30Z", "尝试撤销非最近事件")
	check("revoking a non-last event -> 409 not_last_event",
		code == 409 && e.Error.Code == "not_last_event", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	_, b = getBatch(bn)
	check("non-last revoke changes nothing",
		b.State == "in" && b.AccumulatedSeconds == 10, fmt.Sprintf("state=%s acc=%d", b.State, b.AccumulatedSeconds))
	check("non-last revoke wrote no audit", func() bool {
		for _, ev := range listEvents(bn) {
			if ev.RevokedAt != nil {
				return false
			}
		}
		return true
	}())

	// Revoke the last event (the return) then the same id must be rejected as
	// already revoked, and the new last (the takeout) cannot be revoked with a
	// time not later than the revocation.
	code, _, _ = revokeEvent(bn, nReturnID, "2026-04-01T00:00:40Z", "撤销归还")
	check("revoke last return succeeds", code == 200, fmt.Sprintf("status=%d", code))
	code, _, e = revokeEvent(bn, nReturnID, "2026-04-01T00:00:50Z", "重复撤销")
	check("duplicate revocation -> 409 already_revoked",
		code == 409 && e.Error.Code == "already_revoked", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, _, e = revokeEvent(bn, nTakeoutID, "2026-04-01T00:00:40Z", "撤销时刻不晚于最后操作")
	check("revocation time not later than last op -> 409 time_not_monotonic",
		code == 409 && e.Error.Code == "time_not_monotonic", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, _, e = revokeEvent(bn, nTakeoutID, "2026-04-01T00:00:30", "bad time format")
	check("revocation bad time -> 400 invalid_time",
		code == 400 && e.Error.Code == "invalid_time", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, _, e = revokeEvent(bn, nTakeoutID, "2026-04-01T00:00:50Z", "   ")
	check("revocation empty reason -> 400 reason_required",
		code == 400 && e.Error.Code == "reason_required", fmt.Sprintf("status=%d code=%s", code, e.Error.Code))
	code, _, e = revokeEvent("VERIFY-NO-SUCH-"+strconv.FormatInt(uniq, 10), 1, "2026-04-01T00:00:50Z", "x")
	check("revocation on unknown batch -> 404", code == 404 && e.Error.Code == "not_found",
		fmt.Sprintf("status=%d code=%s", code, e.Error.Code))

	// 8. Concurrency: two stations revoking the same event — exactly one wins.
	bc := fmt.Sprintf("VERIFY-C-%d", uniq)
	createBatch(bc, 100, "2026-05-01T00:00:00Z")
	postEvent(bc, "takeout", "2026-05-01T00:00:10Z")
	cEv0 := listEvents(bc)
	requiref("revoke setup: one takeout event", len(cEv0) == 1, fmt.Sprintf("events=%d", len(cEv0)))
	cTakeoutID := cEv0[0].ID
	rcodes := make([]int, racers)
	var rwg sync.WaitGroup
	for i := 0; i < racers; i++ {
		rwg.Add(1)
		go func(i int) {
			defer rwg.Done()
			rcodes[i], _ = revokeRaw(bc, cTakeoutID, "2026-05-01T00:00:20Z", "两个工位并发撤销")
		}(i)
	}
	rwg.Wait()
	check("racing revocations: exactly one wins", countCode(rcodes, 200) == 1 && countCode(rcodes, 409) == racers-1,
		fmt.Sprintf("codes=%v", rcodes))
	_, b = getBatch(bc)
	cEv := listEvents(bc)
	auditCount := 0
	for _, ev := range cEv {
		if ev.RevokedAt != nil {
			auditCount++
		}
	}
	check("racing revocation leaves batch in-cabinet with one audit row",
		b.State == "in" && b.AccumulatedSeconds == 0 && auditCount == 1 && len(cEv) == 1,
		fmt.Sprintf("state=%s acc=%d audit=%d events=%d", b.State, b.AccumulatedSeconds, auditCount, len(cEv)))

	// 9. Regression: batches/events that never use revocation keep identical
	// shape (no revokedAt/revokeReason keys) and results.
	bk := fmt.Sprintf("VERIFY-K-%d", uniq)
	createBatch(bk, 10, "2026-06-01T00:00:00Z")
	postEvent(bk, "takeout", "2026-06-01T00:00:05Z")
	code, b, _ = postEvent(bk, "return", "2026-06-01T00:00:15Z")
	check("no-revoke critical boundary flow unchanged",
		code == 201 && b.AccumulatedSeconds == 10 && b.Status == "usable" &&
			b.LastEvent != nil && b.LastEvent.RevokedAt == nil && b.LastEvent.RevokeReason == nil,
		fmt.Sprintf("acc=%d status=%s", b.AccumulatedSeconds, b.Status))

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
