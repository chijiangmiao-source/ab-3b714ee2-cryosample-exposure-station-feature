// Package app wires the HTTP API (Gin) on top of the SQLite store.
package app

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"timingstation/api/internal/store"
)

// Server holds the dependencies of the HTTP API.
type Server struct {
	st *store.Store
}

// NewRouter builds the Gin engine with all API routes under /api.
func NewRouter(st *store.Store) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), cors())

	s := &Server{st: st}
	api := r.Group("/api")
	api.GET("/health", s.health)
	api.POST("/batches", s.createBatch)
	api.GET("/batches/:barcode", s.getBatch)
	api.GET("/batches/:barcode/events", s.listEvents)
	api.POST("/batches/:barcode/events", s.createEvent)
	api.POST("/batches/:barcode/events/:id/revocation", s.revokeEvent)
	return r
}

func cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// zTimeRe enforces the canonical event-time format: RFC3339, whole seconds,
// UTC "Z" suffix — e.g. 2026-09-13T10:00:00Z. No offsets, no fractions.
var zTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func parseEventTime(s string) (time.Time, error) {
	if !zTimeRe.MatchString(s) {
		return time.Time{}, errInvalidTime
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errInvalidTime
	}
	return t.UTC(), nil
}

var errInvalidTime = errors.New("time must be RFC3339 whole seconds with Z suffix, e.g. 2026-09-13T10:00:00Z")

func writeErr(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": msg}})
}

func (s *Server) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type createBatchReq struct {
	Barcode        string `json:"barcode"`
	AllowedSeconds int64  `json:"allowedSeconds"`
	CreatedAt      string `json:"createdAt"`
}

func (s *Server) createBatch(c *gin.Context) {
	var req createBatchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	req.Barcode = strings.TrimSpace(req.Barcode)
	if req.Barcode == "" {
		writeErr(c, http.StatusBadRequest, "barcode_required", "barcode must be a non-empty string")
		return
	}
	if len(req.Barcode) > 128 {
		writeErr(c, http.StatusBadRequest, "barcode_too_long", "barcode must be at most 128 characters")
		return
	}
	if req.AllowedSeconds <= 0 {
		writeErr(c, http.StatusBadRequest, "invalid_allowed_seconds", "allowedSeconds must be a positive integer")
		return
	}
	createdAt, err := parseEventTime(req.CreatedAt)
	if err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	b, err := s.st.CreateBatch(c.Request.Context(), req.Barcode, req.AllowedSeconds, createdAt)
	if errors.Is(err, store.ErrDuplicate) {
		writeErr(c, http.StatusConflict, "duplicate_barcode", "a batch with this barcode already exists")
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusCreated, b)
}

func (s *Server) getBatch(c *gin.Context) {
	b, err := s.st.GetBatch(c.Request.Context(), c.Param("barcode"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusOK, b)
}

func (s *Server) listEvents(c *gin.Context) {
	events, err := s.st.ListEvents(c.Request.Context(), c.Param("barcode"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]eventJSON, 0, len(events))
	for _, ev := range events {
		out = append(out, toEventJSON(&ev))
	}
	c.JSON(http.StatusOK, gin.H{"events": out})
}

type createEventReq struct {
	Type string `json:"type"`
	At   string `json:"at"`
}

func (s *Server) createEvent(c *gin.Context) {
	barcode := c.Param("barcode")
	var req createEventReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.Type != store.EventTakeout && req.Type != store.EventReturn {
		writeErr(c, http.StatusBadRequest, "invalid_event_type", "type must be \"takeout\" or \"return\"")
		return
	}
	at, err := parseEventTime(req.At)
	if err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	b, err := s.st.ApplyEvent(c.Request.Context(), barcode, req.Type, at)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no batch with this barcode")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusCreated, b)
}

type revokeEventReq struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// revokeEvent handles POST /batches/:barcode/events/:id/revocation. Only the
// last non-revoked event may be undone; the audit-carrying request requires a
// canonical whole-second Z time and a non-empty reason.
func (s *Server) revokeEvent(c *gin.Context) {
	barcode := c.Param("barcode")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(c, http.StatusBadRequest, "invalid_event_id", "event id must be a positive integer")
		return
	}
	var req revokeEventReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErr(c, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	at, err := parseEventTime(req.At)
	if err != nil {
		writeErr(c, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeErr(c, http.StatusBadRequest, "reason_required", "reason must be a non-empty string")
		return
	}
	if len(reason) > 500 {
		writeErr(c, http.StatusBadRequest, "reason_too_long", "reason must be at most 500 characters")
		return
	}
	b, err := s.st.RevokeEvent(c.Request.Context(), barcode, id, at, reason)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrEventNotFound) {
		writeErr(c, http.StatusNotFound, "not_found", "no event with this id for this batch")
		return
	}
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		writeErr(c, http.StatusConflict, ce.Code, ce.Message)
		return
	}
	if err != nil {
		writeErr(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.respondBatch(c, http.StatusOK, b)
}

// respondBatch renders the batch together with its latest active event (if any).
func (s *Server) respondBatch(c *gin.Context, status int, b *store.Batch) {
	var last *eventJSON
	if ev, err := s.st.LastEvent(c.Request.Context(), b.Barcode); err == nil && ev != nil {
		e := toEventJSON(ev)
		last = &e
	}
	c.JSON(status, batchJSON{
		Barcode:            b.Barcode,
		AllowedSeconds:     b.AllowedSeconds,
		State:              b.State,
		Status:             b.Status,
		Usable:             b.Status == store.StatusUsable,
		AccumulatedSeconds: b.AccumulatedSeconds,
		RemainingSeconds:   b.AllowedSeconds - b.AccumulatedSeconds,
		CreatedAt:          b.CreatedAt.UTC().Format(time.RFC3339),
		LastEvent:          last,
	})
}

type eventJSON struct {
	ID           int64  `json:"id"`
	Type         string `json:"type"`
	At           string `json:"at"`
	DeltaSeconds *int64 `json:"deltaSeconds"`
	// RevokedAt/RevokeReason are present only for revoked events; older
	// clients that ignore unknown fields keep working unchanged.
	RevokedAt    *string `json:"revokedAt,omitempty"`
	RevokeReason *string `json:"revokeReason,omitempty"`
}

func toEventJSON(ev *store.Event) eventJSON {
	j := eventJSON{
		ID:           ev.ID,
		Type:         ev.Type,
		At:           ev.At.UTC().Format(time.RFC3339),
		DeltaSeconds: ev.DeltaSeconds,
	}
	if ev.RevokedAt != nil {
		s := ev.RevokedAt.UTC().Format(time.RFC3339)
		j.RevokedAt = &s
	}
	if ev.RevokeReason != nil {
		r := *ev.RevokeReason
		j.RevokeReason = &r
	}
	return j
}

type batchJSON struct {
	Barcode            string     `json:"barcode"`
	AllowedSeconds     int64      `json:"allowedSeconds"`
	State              string     `json:"state"`  // "in" | "out"
	Status             string     `json:"status"` // "usable" | "scrapped"
	Usable             bool       `json:"usable"` // status == "usable"
	AccumulatedSeconds int64      `json:"accumulatedSeconds"`
	RemainingSeconds   int64      `json:"remainingSeconds"` // allowedSeconds - accumulatedSeconds
	CreatedAt          string     `json:"createdAt"`
	LastEvent          *eventJSON `json:"lastEvent"`
}
