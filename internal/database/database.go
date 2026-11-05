// Package database provides the SQLite (WAL) store for RelayDeck.
// Events are acknowledged only after the insert transaction commits.
// Deliveries are claimed with a lease (claimed_at/claimed_by) so workers
// never process the same delivery concurrently and stale leases recover.
package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS sources (
  name TEXT PRIMARY KEY,
  secret TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS destinations (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  source_filter TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL,
  method TEXT NOT NULL DEFAULT 'POST',
  headers_json TEXT NOT NULL DEFAULT '{}',
  timeout_seconds INTEGER NOT NULL DEFAULT 10,
  max_retries INTEGER NOT NULL DEFAULT 5,
  backoff_base_ms INTEGER NOT NULL DEFAULT 1000,
  backoff_max_ms INTEGER NOT NULL DEFAULT 60000,
  filter_substring TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  payload BLOB NOT NULL,
  headers_json TEXT NOT NULL DEFAULT '{}',
  content_type TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(source, idempotency_key)
);
CREATE TABLE IF NOT EXISTS deliveries (
  id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL REFERENCES events(id),
  destination_id TEXT NOT NULL REFERENCES destinations(id),
  status TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  claimed_at TEXT,
  claimed_by TEXT,
  last_status_code INTEGER,
  last_error TEXT NOT NULL DEFAULT '',
  last_duration_ms INTEGER NOT NULL DEFAULT 0,
  last_response TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS attempts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  delivery_id TEXT NOT NULL REFERENCES deliveries(id),
  status_code INTEGER,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  error_text TEXT NOT NULL DEFAULT '',
  response_snippet TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deliveries_due ON deliveries(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_deliveries_event ON deliveries(event_id);
CREATE INDEX IF NOT EXISTS idx_deliveries_dest ON deliveries(destination_id);
CREATE INDEX IF NOT EXISTS idx_events_source ON events(source, created_at);
CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at);
CREATE INDEX IF NOT EXISTS idx_attempts_delivery ON attempts(delivery_id);
`

// Event is a stored inbound webhook.
type Event struct {
	ID             string            `json:"id"`
	Source         string            `json:"source"`
	Payload        []byte            `json:"-"`
	PayloadText    string            `json:"payload,omitempty"`
	Headers        map[string]string `json:"headers"`
	ContentType    string            `json:"content_type"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

// Destination is a delivery target.
type Destination struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	SourceFilter   string            `json:"source_filter"`
	URL            string            `json:"url"`
	Method         string            `json:"method"`
	Headers        map[string]string `json:"headers"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	MaxRetries     int               `json:"max_retries"`
	BackoffBaseMs  int64             `json:"backoff_base_ms"`
	BackoffMaxMs   int64             `json:"backoff_max_ms"`
	Filter         string            `json:"filter_substring"`
	Enabled        bool              `json:"enabled"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// Delivery is one event -> one destination delivery job.
type Delivery struct {
	ID              string    `json:"id"`
	EventID         string    `json:"event_id"`
	DestinationID   string    `json:"destination_id"`
	DestinationName string    `json:"destination_name,omitempty"`
	Source          string    `json:"source,omitempty"`
	Status          string    `json:"status"` // pending, in_progress, delivered, failed, dead
	Attempts        int       `json:"attempts"`
	NextAttemptAt   time.Time `json:"next_attempt_at"`
	LastStatusCode  *int      `json:"last_status_code,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	LastDurationMs  int64     `json:"last_duration_ms"`
	LastResponse    string    `json:"last_response,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Attempt is one recorded delivery try.
type Attempt struct {
	ID              int64     `json:"id"`
	DeliveryID      string    `json:"delivery_id"`
	StatusCode      *int      `json:"status_code,omitempty"`
	DurationMs      int64     `json:"duration_ms"`
	Error           string    `json:"error,omitempty"`
	ResponseSnippet string    `json:"response_snippet,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// Stats are overview counts computed from the DB.
type Stats struct {
	TotalEvents     int64   `json:"total_events"`
	Successful      int64   `json:"successful_deliveries"`
	Failed          int64   `json:"failed_deliveries"`
	Pending         int64   `json:"pending_deliveries"`
	Dead            int64   `json:"dead_deliveries"`
	RetryCount      int64   `json:"retry_count"`
	SuccessRate     float64 `json:"delivery_success_rate"`
	TotalDeliveries int64   `json:"total_deliveries"`
}

// Store wraps a *sql.DB.
type Store struct{ DB *sql.DB }

// Open opens (creating) the SQLite DB at path and runs migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; serialises writes safely.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Ping() error  { return s.DB.Ping() }

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	if t.IsZero() {
		t, _ = time.Parse(time.RFC3339, v)
	}
	return t.UTC()
}

func marshalHeaders(h map[string]string) string {
	if h == nil {
		return "{}"
	}
	b, _ := json.Marshal(h)
	return string(b)
}
func unmarshalHeaders(s string) map[string]string {
	m := map[string]string{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

// --- Sources ---

func (s *Store) UpsertSource(name, secret string) error {
	_, err := s.DB.Exec(`INSERT INTO sources(name, secret, created_at) VALUES(?,?,?)
		ON CONFLICT(name) DO UPDATE SET secret=excluded.secret`, name, secret, nowUTC())
	return err
}

func (s *Store) GetSourceSecret(name string) (string, bool, error) {
	var secret string
	err := s.DB.QueryRow(`SELECT secret FROM sources WHERE name=?`, name).Scan(&secret)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return secret, true, nil
}

func (s *Store) ListSources() ([]string, error) {
	rows, err := s.DB.Query(`SELECT name FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// --- Destinations ---

func (s *Store) CreateDestination(d *Destination) error {
	d.CreatedAt, d.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	_, err := s.DB.Exec(`INSERT INTO destinations(id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, d.ID, d.Name, d.SourceFilter, d.URL, d.Method, marshalHeaders(d.Headers), d.TimeoutSeconds, d.MaxRetries, d.BackoffBaseMs, d.BackoffMaxMs, d.Filter, boolToInt(d.Enabled), d.CreatedAt.Format(time.RFC3339Nano), d.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanDestination(rows interface{ Scan(...any) error }) (*Destination, error) {
	var d Destination
	var headers, created, updated string
	var enabled int
	if err := rows.Scan(&d.ID, &d.Name, &d.SourceFilter, &d.URL, &d.Method, &headers, &d.TimeoutSeconds, &d.MaxRetries, &d.BackoffBaseMs, &d.BackoffMaxMs, &d.Filter, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	d.Headers = unmarshalHeaders(headers)
	d.Enabled = enabled == 1
	d.CreatedAt, d.UpdatedAt = parseTime(created), parseTime(updated)
	return &d, nil
}

func (s *Store) GetDestination(id string) (*Destination, error) {
	row := s.DB.QueryRow(`SELECT id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at FROM destinations WHERE id=?`, id)
	d, err := scanDestination(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) GetDestinationByName(name string) (*Destination, error) {
	row := s.DB.QueryRow(`SELECT id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at FROM destinations WHERE name=?`, name)
	d, err := scanDestination(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) ListDestinations() ([]*Destination, error) {
	rows, err := s.DB.Query(`SELECT id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at FROM destinations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) UpdateDestination(d *Destination) error {
	d.UpdatedAt = time.Now().UTC()
	_, err := s.DB.Exec(`UPDATE destinations SET name=?,source_filter=?,url=?,method=?,headers_json=?,timeout_seconds=?,max_retries=?,backoff_base_ms=?,backoff_max_ms=?,filter_substring=?,enabled=?,updated_at=? WHERE id=?`,
		d.Name, d.SourceFilter, d.URL, d.Method, marshalHeaders(d.Headers), d.TimeoutSeconds, d.MaxRetries, d.BackoffBaseMs, d.BackoffMaxMs, d.Filter, boolToInt(d.Enabled), d.UpdatedAt.Format(time.RFC3339Nano), d.ID)
	return err
}

func (s *Store) DeleteDestination(id string) error {
	_, err := s.DB.Exec(`DELETE FROM destinations WHERE id=?`, id)
	return err
}

// MatchingDestinations returns enabled destinations whose source filter
// matches source (empty filter = all) and whose substring filter, if set,
// appears in the payload.
func (s *Store) MatchingDestinations(source string, payload []byte) ([]*Destination, error) {
	all, err := s.ListDestinations()
	if err != nil {
		return nil, err
	}
	var out []*Destination
	for _, d := range all {
		if !d.Enabled {
			continue
		}
		if d.SourceFilter != "" && d.SourceFilter != source {
			continue
		}
		if d.Filter != "" && !strings.Contains(string(payload), d.Filter) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// --- Events ---

// CreateEvent inserts the event and its deliveries in ONE transaction.
// If an idempotency key already exists for this source, the existing event
// is returned with existed=true and no duplicate deliveries are created.
// The caller must only acknowledge the webhook after this returns nil.
func (s *Store) CreateEvent(ev *Event, dests []*Destination) (existed bool, err error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if ev.IdempotencyKey != "" {
		var existing string
		err := tx.QueryRow(`SELECT id FROM events WHERE source=? AND idempotency_key=?`, ev.Source, ev.IdempotencyKey).Scan(&existing)
		if err == nil {
			ev.ID = existing
			return true, tx.Commit()
		}
		if err != sql.ErrNoRows {
			return false, err
		}
	}
	now := nowUTC()
	var keyVal any
	if ev.IdempotencyKey != "" {
		keyVal = ev.IdempotencyKey
	}
	if _, err := tx.Exec(`INSERT INTO events(id,source,payload,headers_json,content_type,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?)`,
		ev.ID, ev.Source, ev.Payload, marshalHeaders(ev.Headers), ev.ContentType, keyVal, now); err != nil {
		return false, err
	}
	ev.CreatedAt = parseTime(now)
	for _, d := range dests {
		delID := "dlv_" + ev.ID[4:] + "_" + d.ID
		if len(delID) > 64 {
			delID = delID[:64]
		}
		if _, err := tx.Exec(`INSERT INTO deliveries(id,event_id,destination_id,status,attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,0,?,?,?)`,
			delID, ev.ID, d.ID, "pending", now, now, now); err != nil {
			return false, err
		}
	}
	return false, tx.Commit()
}

func (s *Store) GetEvent(id string) (*Event, error) {
	var ev Event
	var headers, created string
	var key sql.NullString
	err := s.DB.QueryRow(`SELECT id,source,payload,headers_json,content_type,idempotency_key,created_at FROM events WHERE id=?`, id).
		Scan(&ev.ID, &ev.Source, &ev.Payload, &headers, &ev.ContentType, &key, &created)
	ev.IdempotencyKey = key.String
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ev.Headers = unmarshalHeaders(headers)
	ev.CreatedAt = parseTime(created)
	ev.PayloadText = string(ev.Payload)
	return &ev, nil
}

func (s *Store) ListEvents(limit int, source, status string, search string) ([]*Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT e.id,e.source,e.payload,e.headers_json,e.content_type,e.idempotency_key,e.created_at FROM events e`
	var args []any
	var conds []string
	if source != "" {
		conds = append(conds, "e.source=?")
		args = append(args, source)
	}
	if search != "" {
		conds = append(conds, "e.id LIKE ?")
		args = append(args, "%"+search+"%")
	}
	if status != "" {
		// Filter events that have at least one delivery with this status.
		conds = append(conds, "EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id=e.id AND d.status=?)")
		args = append(args, status)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY e.created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var ev Event
		var headers, created string
		var key sql.NullString
		if err := rows.Scan(&ev.ID, &ev.Source, &ev.Payload, &headers, &ev.ContentType, &key, &created); err != nil {
			return nil, err
		}
		ev.IdempotencyKey = key.String
		ev.Headers = unmarshalHeaders(headers)
		ev.CreatedAt = parseTime(created)
		ev.PayloadText = string(ev.Payload)
		evCopy := ev
		out = append(out, &evCopy)
	}
	return out, rows.Err()
}

// --- Deliveries ---

func scanDelivery(row interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var next, created, updated string
	var claimedAt, claimedBy sql.NullString
	var lastCode sql.NullInt64
	if err := row.Scan(&d.ID, &d.EventID, &d.DestinationID, &d.Status, &d.Attempts, &next, &claimedAt, &claimedBy, &lastCode, &d.LastError, &d.LastDurationMs, &d.LastResponse, &created, &updated); err != nil {
		return nil, err
	}
	d.NextAttemptAt = parseTime(next)
	d.CreatedAt, d.UpdatedAt = parseTime(created), parseTime(updated)
	if lastCode.Valid {
		v := int(lastCode.Int64)
		d.LastStatusCode = &v
	}
	return &d, nil
}

const deliveryCols = `d.id,d.event_id,d.destination_id,d.status,d.attempts,d.next_attempt_at,d.claimed_at,d.claimed_by,d.last_status_code,d.last_error,d.last_duration_ms,d.last_response,d.created_at,d.updated_at`

func (s *Store) GetDelivery(id string) (*Delivery, error) {
	row := s.DB.QueryRow(`SELECT `+deliveryCols+` FROM deliveries d WHERE d.id=?`, id)
	d, err := scanDelivery(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) ListDeliveries(limit int, status, destinationID string) ([]*Delivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + deliveryCols + `, dest.name, e.source FROM deliveries d JOIN destinations dest ON dest.id=d.destination_id JOIN events e ON e.id=d.event_id`
	var args []any
	var conds []string
	if status != "" {
		conds = append(conds, "d.status=?")
		args = append(args, status)
	}
	if destinationID != "" {
		conds = append(conds, "d.destination_id=?")
		args = append(args, destinationID)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY d.created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		var d Delivery
		var next, created, updated string
		var claimedAt, claimedBy sql.NullString
		var lastCode sql.NullInt64
		if err := rows.Scan(&d.ID, &d.EventID, &d.DestinationID, &d.Status, &d.Attempts, &next, &claimedAt, &claimedBy, &lastCode, &d.LastError, &d.LastDurationMs, &d.LastResponse, &created, &updated, &d.DestinationName, &d.Source); err != nil {
			return nil, err
		}
		d.NextAttemptAt = parseTime(next)
		d.CreatedAt, d.UpdatedAt = parseTime(created), parseTime(updated)
		if lastCode.Valid {
			v := int(lastCode.Int64)
			d.LastStatusCode = &v
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

func (s *Store) ListAttempts(deliveryID string) ([]*Attempt, error) {
	rows, err := s.DB.Query(`SELECT id,delivery_id,status_code,duration_ms,error_text,response_snippet,created_at FROM attempts WHERE delivery_id=? ORDER BY id ASC`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Attempt
	for rows.Next() {
		var a Attempt
		var code sql.NullInt64
		var created string
		if err := rows.Scan(&a.ID, &a.DeliveryID, &code, &a.DurationMs, &a.Error, &a.ResponseSnippet, &created); err != nil {
			return nil, err
		}
		if code.Valid {
			v := int(code.Int64)
			a.StatusCode = &v
		}
		a.CreatedAt = parseTime(created)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ClaimDue atomically claims up to limit due deliveries for workerID.
// A delivery is claimable when pending (or in_progress with a stale lease
// older than leaseTimeout) and next_attempt_at <= now. Returns claimed rows.
func (s *Store) ClaimDue(workerID string, limit int, leaseTimeout time.Duration) ([]*Delivery, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	staleBefore := now.Add(-leaseTimeout).Format(time.RFC3339Nano)
	rows, err := tx.Query(`SELECT `+deliveryCols+` FROM deliveries d WHERE d.next_attempt_at<=? AND (d.status='pending' OR (d.status='in_progress' AND d.claimed_at IS NOT NULL AND d.claimed_at<=?)) ORDER BY d.next_attempt_at ASC LIMIT ?`,
		now.Format(time.RFC3339Nano), staleBefore, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	var claimed []*Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, d.ID)
		claimed = append(claimed, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE deliveries SET status='in_progress', claimed_at=?, claimed_by=?, updated_at=? WHERE id=?`,
			now.Format(time.RFC3339Nano), workerID, now.Format(time.RFC3339Nano), id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, d := range claimed {
		d.Status = "in_progress"
	}
	return claimed, nil
}

// RequeueStaleLeases returns in_progress deliveries whose lease expired to pending.
func (s *Store) RequeueStaleLeases(leaseTimeout time.Duration) (int64, error) {
	staleBefore := time.Now().UTC().Add(-leaseTimeout).Format(time.RFC3339Nano)
	res, err := s.DB.Exec(`UPDATE deliveries SET status='pending', claimed_at=NULL, claimed_by=NULL, updated_at=? WHERE status='in_progress' AND claimed_at IS NOT NULL AND claimed_at<=?`,
		nowUTC(), staleBefore)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecordAttempt records one try and schedules the next state.
// On success (2xx): status=delivered. On failure: attempts incremented;
// if attempts >= maxRetries -> dead, else pending with next_attempt_at=now+delay.
func (s *Store) RecordAttempt(deliveryID string, statusCode *int, durationMs int64, errText, responseSnippet string, success bool, maxRetries int, nextDelay time.Duration) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var code any
	if statusCode != nil {
		code = *statusCode
	}
	if _, err := tx.Exec(`INSERT INTO attempts(delivery_id,status_code,duration_ms,error_text,response_snippet,created_at) VALUES(?,?,?,?,?,?)`,
		deliveryID, code, durationMs, errText, responseSnippet, nowUTC()); err != nil {
		return err
	}
	var attempts int
	if err := tx.QueryRow(`SELECT attempts FROM deliveries WHERE id=?`, deliveryID).Scan(&attempts); err != nil {
		return err
	}
	attempts++
	now := time.Now().UTC()
	var status string
	var next time.Time
	if success {
		status, next = "delivered", now
	} else if attempts >= maxRetries {
		status, next = "dead", now
	} else {
		status, next = "pending", now.Add(nextDelay)
	}
	if _, err := tx.Exec(`UPDATE deliveries SET status=?, attempts=?, next_attempt_at=?, claimed_at=NULL, claimed_by=NULL, last_status_code=?, last_error=?, last_duration_ms=?, last_response=?, updated_at=? WHERE id=?`,
		status, attempts, next.Format(time.RFC3339Nano), code, errText, durationMs, responseSnippet, now.Format(time.RFC3339Nano), deliveryID); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryDelivery resets a failed/dead delivery to pending for immediate retry.
func (s *Store) RetryDelivery(id string) (bool, error) {
	res, err := s.DB.Exec(`UPDATE deliveries SET status='pending', next_attempt_at=?, claimed_at=NULL, claimed_by=NULL, updated_at=? WHERE id=? AND status IN ('failed','dead','pending')`,
		nowUTC(), nowUTC(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReplayEvent creates fresh pending deliveries for an event to all currently
// matching destinations.
func (s *Store) ReplayEvent(eventID string, dests []*Destination) (int, error) {
	now := nowUTC()
	count := 0
	for i, d := range dests {
		delID := fmt.Sprintf("rpl_%d_%d_%s", time.Now().UnixNano(), i, d.ID)
		if len(delID) > 64 {
			delID = delID[:64]
		}
		res, err := s.DB.Exec(`INSERT OR IGNORE INTO deliveries(id,event_id,destination_id,status,attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,0,?,?,?)`,
			delID, eventID, d.ID, "pending", now, now, now)
		if err != nil {
			return count, err
		}
		n, _ := res.RowsAffected()
		count += int(n)
	}
	return count, nil
}

// Stats computes overview counts.
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{}
	row := s.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM events),
		(SELECT COUNT(*) FROM deliveries WHERE status='delivered'),
		(SELECT COUNT(*) FROM deliveries WHERE status IN ('failed','dead')),
		(SELECT COUNT(*) FROM deliveries WHERE status IN ('pending','in_progress')),
		(SELECT COUNT(*) FROM deliveries WHERE status='dead'),
		(SELECT COALESCE(SUM(CASE WHEN attempts>0 THEN attempts-1 ELSE 0 END),0) FROM deliveries),
		(SELECT COUNT(*) FROM deliveries)`)
	var failed int64
	if err := row.Scan(&st.TotalEvents, &st.Successful, &failed, &st.Pending, &st.Dead, &st.RetryCount, &st.TotalDeliveries); err != nil {
		return nil, err
	}
	st.Failed = failed
	if st.TotalDeliveries > 0 {
		st.SuccessRate = float64(st.Successful) / float64(st.TotalDeliveries) * 100
	}
	return st, nil
}

// PendingCount returns the number of pending/in-progress deliveries (for /metrics gauge).
func (s *Store) PendingCount() int64 {
	var n int64
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE status IN ('pending','in_progress')`).Scan(&n)
	return n
}

// DeleteOldEvents removes events older than retention days (0 = disabled).
func (s *Store) DeleteOldEvents(retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339Nano)
	res, err := s.DB.Exec(`DELETE FROM events WHERE created_at<?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
