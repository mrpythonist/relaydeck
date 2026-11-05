// Package api implements the RelayDeck HTTP server: webhook ingestion,
// destination/event/delivery management, health/metrics and the dashboard.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mrpythonist/relaydeck/internal/auth"
	"github.com/mrpythonist/relaydeck/internal/config"
	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/delivery"
	"github.com/mrpythonist/relaydeck/internal/events"
	"github.com/mrpythonist/relaydeck/internal/metrics"
	"github.com/mrpythonist/relaydeck/internal/security"
	web "github.com/mrpythonist/relaydeck/web"
)

// Server is the HTTP server.
type Server struct {
	Store   *database.Store
	Config  *config.Config
	Metrics *metrics.Metrics
	Sender  *delivery.Sender
	mux     *http.ServeMux
	log     *slog.Logger
}

// New builds a Server.
func New(store *database.Store, cfg *config.Config, m *metrics.Metrics) *Server {
	s := &Server{Store: store, Config: cfg, Metrics: m, log: slog.Default()}
	s.Sender = delivery.NewSender(cfg.AllowPrivate, cfg.TrustedInternal)
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /health", s.handleHealth)
	m.HandleFunc("GET /ready", s.handleReady)
	m.HandleFunc("GET /metrics", s.handleMetrics)
	m.HandleFunc("POST /api/v1/webhooks/{source}", s.handleIngest)

	// Management API: API-key enforced when configured.
	api := auth.RequireAPIKey(s.Config.APIKey, http.HandlerFunc(s.handleAPI))
	m.Handle("/api/v1/", api)

	// Dashboard.
	m.HandleFunc("GET /{$}", s.handleDashboard)
	m.HandleFunc("GET /dashboard", s.handleDashboard)
	m.HandleFunc("GET /static/", s.handleStatic)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) logReq(r *http.Request, code int, dur time.Duration) {
	s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", code, "duration_ms", dur.Milliseconds())
}

// --- health / metrics ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(s.Metrics.Render(s.Store.PendingCount())))
}

// --- dashboard ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(web.FS, "templates/index.html")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "dashboard unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	b, err := fs.ReadFile(web.FS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript")
	}
	_, _ = w.Write(b)
}

// --- ingestion ---

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	source := events.NormaliseSource(r.PathValue("source"))
	if err := events.ValidateSourceName(source); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.Config.MaxPayloadBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read body")
		return
	}
	if int64(len(body)) > s.Config.MaxPayloadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("payload exceeds limit of %d bytes", s.Config.MaxPayloadBytes))
		return
	}
	// Source secret: HMAC verification when configured.
	secret, found, err := s.Store.GetSourceSecret(source)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		// Auto-register unknown sources with no secret.
		_ = s.Store.UpsertSource(source, "")
	}
	if secret != "" {
		sig := r.Header.Get("X-RelayDeck-Signature")
		if sig == "" {
			sig = r.Header.Get("X-Hub-Signature-256")
		}
		if !security.VerifySignature(secret, body, sig) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing signature")
			return
		}
	}
	// Collect + redact headers.
	headers := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}
	headers = security.RedactHeaders(headers)
	id, err := security.NewEventID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	ev := &database.Event{
		ID:             id,
		Source:         source,
		Payload:        body,
		Headers:        headers,
		ContentType:    r.Header.Get("Content-Type"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	dests, err := s.Store.MatchingDestinations(source, body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	existed, err := s.Store.CreateEvent(ev, dests)
	if err != nil {
		s.log.Error("create event failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to persist event")
		return
	}
	if !existed {
		s.Metrics.EventsReceived.Add(1)
	}
	s.logReq(r, http.StatusAccepted, time.Since(start))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"event_id":          ev.ID,
		"status":            "accepted",
		"duplicate":         existed,
		"deliveries_queued": len(dests),
	})
}

// --- management API ---

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/api/v1/destinations" && r.Method == http.MethodGet:
		s.listDestinations(w, r)
	case path == "/api/v1/destinations" && r.Method == http.MethodPost:
		s.createDestination(w, r)
	case strings.HasPrefix(path, "/api/v1/destinations/") && r.Method == http.MethodGet:
		s.getDestination(w, r)
	case strings.HasPrefix(path, "/api/v1/destinations/") && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
		s.updateDestination(w, r)
	case strings.HasPrefix(path, "/api/v1/destinations/") && r.Method == http.MethodDelete:
		s.deleteDestination(w, r)
	case path == "/api/v1/events" && r.Method == http.MethodGet:
		s.listEvents(w, r)
	case strings.HasPrefix(path, "/api/v1/events/") && strings.HasSuffix(path, "/replay") && r.Method == http.MethodPost:
		s.replayEvent(w, r)
	case strings.HasPrefix(path, "/api/v1/events/") && r.Method == http.MethodGet:
		s.getEvent(w, r)
	case path == "/api/v1/deliveries" && r.Method == http.MethodGet:
		s.listDeliveries(w, r)
	case strings.HasPrefix(path, "/api/v1/deliveries/") && strings.HasSuffix(path, "/retry") && r.Method == http.MethodPost:
		s.retryDelivery(w, r)
	case strings.HasPrefix(path, "/api/v1/deliveries/") && r.Method == http.MethodGet:
		s.getDelivery(w, r)
	case path == "/api/v1/stats" && r.Method == http.MethodGet:
		s.getStats(w, r)
	case path == "/api/v1/sources" && r.Method == http.MethodGet:
		s.listSources(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func maskDestination(d *database.Destination) *database.Destination {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Headers = map[string]string{}
	for k, v := range d.Headers {
		if security.IsSensitiveHeader(k) {
			cp.Headers[k] = "***REDACTED***"
		} else {
			cp.Headers[k] = v
		}
	}
	return &cp
}

func (s *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	dests, err := s.Store.ListDestinations()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]*database.Destination, 0, len(dests))
	for _, d := range dests {
		out = append(out, maskDestination(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"destinations": out})
}

func (s *Server) createDestination(w http.ResponseWriter, r *http.Request) {
	var d database.Destination
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(d.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := security.ValidateDestinationURL(d.URL, s.Config.AllowPrivate, s.Config.TrustedInternal, nil); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid destination url: "+err.Error())
		return
	}
	if d.Method == "" {
		d.Method = "POST"
	}
	d.Method = strings.ToUpper(d.Method)
	if d.TimeoutSeconds <= 0 {
		d.TimeoutSeconds = 10
	}
	if d.MaxRetries <= 0 {
		d.MaxRetries = 5
	}
	if d.BackoffBaseMs <= 0 {
		d.BackoffBaseMs = 1000
	}
	if d.BackoffMaxMs <= 0 {
		d.BackoffMaxMs = 60000
	}
	if d.Headers == nil {
		d.Headers = map[string]string{}
	}
	d.Enabled = true
	id, err := security.NewID("dst_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	d.ID = id
	if err := s.Store.CreateDestination(&d); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusConflict, "destination name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, maskDestination(&d))
}

func (s *Server) destID(r *http.Request) string {
	return strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/retry"), "/api/v1/destinations/")
}

func (s *Server) getDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDestination(s.destID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeErr(w, http.StatusNotFound, "destination not found")
		return
	}
	writeJSON(w, http.StatusOK, maskDestination(d))
}

func (s *Server) updateDestination(w http.ResponseWriter, r *http.Request) {
	id := s.destID(r)
	existing, err := s.Store.GetDestination(id)
	if err != nil || existing == nil {
		writeErr(w, http.StatusNotFound, "destination not found")
		return
	}
	var d database.Destination
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if d.URL != "" && d.URL != existing.URL {
		if err := security.ValidateDestinationURL(d.URL, s.Config.AllowPrivate, s.Config.TrustedInternal, nil); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid destination url: "+err.Error())
			return
		}
		existing.URL = d.URL
	}
	if d.Name != "" {
		existing.Name = d.Name
	}
	if d.SourceFilter != "" || r.URL.Query().Has("clear_source") {
		existing.SourceFilter = d.SourceFilter
	}
	if d.Method != "" {
		existing.Method = strings.ToUpper(d.Method)
	}
	if d.TimeoutSeconds > 0 {
		existing.TimeoutSeconds = d.TimeoutSeconds
	}
	if d.MaxRetries > 0 {
		existing.MaxRetries = d.MaxRetries
	}
	if d.BackoffBaseMs > 0 {
		existing.BackoffBaseMs = d.BackoffBaseMs
	}
	if d.BackoffMaxMs > 0 {
		existing.BackoffMaxMs = d.BackoffMaxMs
	}
	if d.Filter != "" {
		existing.Filter = d.Filter
	}
	// Headers: merge; "***REDACTED***" keeps the stored value.
	if d.Headers != nil {
		if existing.Headers == nil {
			existing.Headers = map[string]string{}
		}
		for k, v := range d.Headers {
			if v == "***REDACTED***" {
				continue
			}
			existing.Headers[k] = v
		}
	}
	// PUT semantics: the payload is the full destination object, so
	// `enabled` comes from the payload (clients must send it explicitly).
	existing.Enabled = d.Enabled
	if err := s.Store.UpdateDestination(existing); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, maskDestination(existing))
}

func (s *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteDestination(s.destID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseLimit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	evs, err := s.Store.ListEvents(parseLimit(r), q.Get("source"), q.Get("status"), q.Get("search"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evs == nil {
		evs = []*database.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

func eventID(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/events/")
	return strings.TrimSuffix(p, "/replay")
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.Store.GetEvent(eventID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ev == nil {
		writeErr(w, http.StatusNotFound, "event not found")
		return
	}
	dels, _ := s.Store.ListDeliveries(100, "", "")
	var mine []*database.Delivery
	for _, d := range dels {
		if d.EventID == ev.ID {
			mine = append(mine, d)
		}
	}
	// Attempts per delivery.
	type deliveryWithAttempts struct {
		*database.Delivery
		AttemptsList []*database.Attempt `json:"attempts_list"`
	}
	var out []deliveryWithAttempts
	for _, d := range mine {
		atts, _ := s.Store.ListAttempts(d.ID)
		out = append(out, deliveryWithAttempts{d, atts})
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev, "deliveries": out})
}

func (s *Server) replayEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.Store.GetEvent(eventID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ev == nil {
		writeErr(w, http.StatusNotFound, "event not found")
		return
	}
	dests, err := s.Store.MatchingDestinations(ev.Source, ev.Payload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := s.Store.ReplayEvent(ev.ID, dests)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_id": ev.ID, "deliveries_created": n})
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dels, err := s.Store.ListDeliveries(parseLimit(r), q.Get("status"), q.Get("destination_id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if dels == nil {
		dels = []*database.Delivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": dels})
}

func deliveryID(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/deliveries/")
	return strings.TrimSuffix(p, "/retry")
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDelivery(deliveryID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeErr(w, http.StatusNotFound, "delivery not found")
		return
	}
	atts, _ := s.Store.ListAttempts(d.ID)
	writeJSON(w, http.StatusOK, map[string]any{"delivery": d, "attempts": atts})
}

func (s *Server) retryDelivery(w http.ResponseWriter, r *http.Request) {
	ok, err := s.Store.RetryDelivery(deliveryID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "delivery not found or not retryable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued for retry"})
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	names, _ := s.Store.ListSources()
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": names})
}
