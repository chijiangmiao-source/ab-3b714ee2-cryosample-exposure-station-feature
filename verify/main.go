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
		Type         string `json:"type"`
		At           string `json:"at"`
		DeltaSeconds *int64 `json:"deltaSeconds"`
	} `json:"lastEvent"`
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
