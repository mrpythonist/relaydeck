// Package database provides the SQLite (WAL) store for RelayDeck.
// Events are acknowledged only after the insert transaction commits.
// Deliveries are claimed with a lease (claimed_at/claimed_by) so workers
// never process the same delivery concurrently and stale leases recover.
//
// Phase 1 hosted: every row that belongs to a customer carries a
// workspace_id. The empty workspace ("") is the legacy single-tenant
// mode: existing self-hosted installs keep working with no change.
// In hosted mode (RELAYDECK_HOSTED=true) all API access must go through
// the ForWorkspace variants; the unscoped wrappers below only touch
// workspace "" and never leak another workspace's rows.
package database

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY,
  public_id TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  owner_user_id TEXT NOT NULL DEFAULT '',
  plan TEXT NOT NULL DEFAULT 'free',
  stripe_customer_id TEXT NOT NULL DEFAULT '',
  stripe_subscription_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS workspace_memberships (
  workspace_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'member',
  created_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, user_id)
);
CREATE TABLE IF NOT EXISTS workspace_api_keys (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  key_hash TEXT NOT NULL UNIQUE,
  key_prefix TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  last_used_at TEXT,
  revoked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_monthly (
  workspace_id TEXT NOT NULL,
  month TEXT NOT NULL,
  event_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (workspace_id, month)
);
CREATE TABLE IF NOT EXISTS sources (
  workspace_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  secret TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (workspace_id, name)
);
CREATE TABLE IF NOT EXISTS destinations (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
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
  updated_at TEXT NOT NULL,
  UNIQUE(workspace_id, name)
);
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL,
  payload BLOB NOT NULL,
  headers_json TEXT NOT NULL DEFAULT '{}',
  content_type TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(workspace_id, source, idempotency_key)
);
CREATE TABLE IF NOT EXISTS deliveries (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL DEFAULT '',
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
-- Workspace indexes (idx_deliveries_ws, idx_events_ws, idx_destinations_ws)
-- are created by migrateHosted after the workspace_id columns exist,
-- so pre-Phase-1 databases do not fail here on a missing column.
CREATE INDEX IF NOT EXISTS idx_api_keys_ws ON workspace_api_keys(workspace_id);
`

// Event is a stored inbound webhook.
type Event struct {
	ID             string            `json:"id"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
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
	WorkspaceID    string            `json:"workspace_id,omitempty"`
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
	WorkspaceID     string    `json:"workspace_id,omitempty"`
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

// User is a hosted account holder.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Workspace is one hosted tenant.
type Workspace struct {
	ID                   string    `json:"id"`
	PublicID             string    `json:"public_id"`
	Name                 string    `json:"name"`
	OwnerUserID          string    `json:"owner_user_id"`
	Plan                 string    `json:"plan"`
	StripeCustomerID     string    `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string    `json:"stripe_subscription_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// WorkspaceAPIKey is a workspace-scoped key. Only the prefix is ever
// returned by the API; the full key is shown once, at creation.
type WorkspaceAPIKey struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	KeyPrefix   string    `json:"key_prefix"`
	CreatedAt   time.Time `json:"created_at"`
	Revoked     bool      `json:"revoked"`
}

// Usage is one workspace's event count for one calendar month (YYYY-MM).
type Usage struct {
	WorkspaceID string `json:"workspace_id"`
	Month       string `json:"month"`
	EventCount  int64  `json:"event_count"`
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
	s := &Store{DB: db}
	if err := s.migrateHosted(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate hosted: %w", err)
	}
	return s, nil
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

// HashToken returns hex(sha256(token)) for storing secrets at rest.
// The plain token is never stored; only its hash is compared.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func currentMonth() string { return time.Now().UTC().Format("2006-01") }

// --- Hosted migration for pre-Phase-1 databases ---
//
// Fresh databases get the v2 schema above. Databases created by v0.1.0
// have no workspace_id columns and global UNIQUE keys on sources,
// destinations.name and events(source, idempotency_key). This migrates
// them in place: add the columns, then rebuild the three tables with
// workspace-scoped keys, keeping every row with workspace_id "".

func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.DB.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *Store) tableSQL(table string) string {
	var sqlText string
	_ = s.DB.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&sqlText)
	return sqlText
}

func (s *Store) addColumnIfMissing(table, col, def string) error {
	cols, err := s.tableColumns(table)
	if err != nil {
		return err
	}
	if cols[col] {
		return nil
	}
	_, err = s.DB.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
}

func (s *Store) migrateHosted() error {
	// Add workspace_id to tables that predate it.
	for _, t := range []struct{ table, def string }{
		{"sources", "workspace_id TEXT NOT NULL DEFAULT ''"},
		{"destinations", "workspace_id TEXT NOT NULL DEFAULT ''"},
		{"events", "workspace_id TEXT NOT NULL DEFAULT ''"},
		{"deliveries", "workspace_id TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.addColumnIfMissing(t.table, "workspace_id", t.def); err != nil {
			return err
		}
	}
	// Rebuild tables whose old global UNIQUE/PK would block two workspaces
	// using the same source name, destination name, or idempotency key.
	if sqlText := s.tableSQL("sources"); sqlText != "" && !strings.Contains(sqlText, "PRIMARY KEY (workspace_id, name)") {
		if _, err := s.DB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			return err
		}
		_, err := s.DB.Exec(`CREATE TABLE sources_new (workspace_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL, secret TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, PRIMARY KEY (workspace_id, name))`)
		if err == nil {
			_, err = s.DB.Exec(`INSERT OR IGNORE INTO sources_new(workspace_id, name, secret, created_at) SELECT workspace_id, name, secret, created_at FROM sources`)
		}
		if err == nil {
			_, err = s.DB.Exec(`DROP TABLE sources`)
		}
		if err == nil {
			_, err = s.DB.Exec(`ALTER TABLE sources_new RENAME TO sources`)
		}
		_, _ = s.DB.Exec(`PRAGMA foreign_keys=ON`)
		if err != nil {
			return fmt.Errorf("rebuild sources: %w", err)
		}
	}
	if sqlText := s.tableSQL("destinations"); sqlText != "" && !strings.Contains(sqlText, "UNIQUE(workspace_id, name)") {
		if _, err := s.DB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			return err
		}
		_, err := s.DB.Exec(`CREATE TABLE destinations_new (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL, source_filter TEXT NOT NULL DEFAULT '', url TEXT NOT NULL, method TEXT NOT NULL DEFAULT 'POST', headers_json TEXT NOT NULL DEFAULT '{}', timeout_seconds INTEGER NOT NULL DEFAULT 10, max_retries INTEGER NOT NULL DEFAULT 5, backoff_base_ms INTEGER NOT NULL DEFAULT 1000, backoff_max_ms INTEGER NOT NULL DEFAULT 60000, filter_substring TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(workspace_id, name))`)
		if err == nil {
			_, err = s.DB.Exec(`INSERT OR IGNORE INTO destinations_new(id,workspace_id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at) SELECT id,workspace_id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at FROM destinations`)
		}
		if err == nil {
			_, err = s.DB.Exec(`DROP TABLE destinations`)
		}
		if err == nil {
			_, err = s.DB.Exec(`ALTER TABLE destinations_new RENAME TO destinations`)
		}
		_, _ = s.DB.Exec(`PRAGMA foreign_keys=ON`)
		if err != nil {
			return fmt.Errorf("rebuild destinations: %w", err)
		}
	}
	if sqlText := s.tableSQL("events"); sqlText != "" && !strings.Contains(sqlText, "UNIQUE(workspace_id, source, idempotency_key)") {
		if _, err := s.DB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			return err
		}
		_, err := s.DB.Exec(`CREATE TABLE events_new (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, payload BLOB NOT NULL, headers_json TEXT NOT NULL DEFAULT '{}', content_type TEXT NOT NULL DEFAULT '', idempotency_key TEXT, created_at TEXT NOT NULL, UNIQUE(workspace_id, source, idempotency_key))`)
		if err == nil {
			_, err = s.DB.Exec(`INSERT OR IGNORE INTO events_new(id,workspace_id,source,payload,headers_json,content_type,idempotency_key,created_at) SELECT id,workspace_id,source,payload,headers_json,content_type,idempotency_key,created_at FROM events`)
		}
		if err == nil {
			_, err = s.DB.Exec(`DROP TABLE events`)
		}
		if err == nil {
			_, err = s.DB.Exec(`ALTER TABLE events_new RENAME TO events`)
		}
		_, _ = s.DB.Exec(`PRAGMA foreign_keys=ON`)
		if err != nil {
			return fmt.Errorf("rebuild events: %w", err)
		}
	}
	// Re-apply indexes that may have been lost in a rebuild.
	_, err := s.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_deliveries_due ON deliveries(status, next_attempt_at);
	CREATE INDEX IF NOT EXISTS idx_deliveries_event ON deliveries(event_id);
	CREATE INDEX IF NOT EXISTS idx_deliveries_dest ON deliveries(destination_id);
	CREATE INDEX IF NOT EXISTS idx_deliveries_ws ON deliveries(workspace_id, status, created_at);
	CREATE INDEX IF NOT EXISTS idx_events_source ON events(source, created_at);
	CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at);
	CREATE INDEX IF NOT EXISTS idx_events_ws ON events(workspace_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_attempts_delivery ON attempts(delivery_id);
	CREATE INDEX IF NOT EXISTS idx_destinations_ws ON destinations(workspace_id, name);
	CREATE INDEX IF NOT EXISTS idx_api_keys_ws ON workspace_api_keys(workspace_id)`)
	return err
}

// --- Users ---

func (s *Store) CreateUser(u *User) error {
	u.CreatedAt, u.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	_, err := s.DB.Exec(`INSERT INTO users(id,email,password_hash,name,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		u.ID, strings.ToLower(strings.TrimSpace(u.Email)), u.PasswordHash, u.Name, u.CreatedAt.Format(time.RFC3339Nano), u.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetUserByEmail(email string) (*User, error) {
	var u User
	var created, updated string
	err := s.DB.QueryRow(`SELECT id,email,password_hash,name,created_at,updated_at FROM users WHERE email=?`, strings.ToLower(strings.TrimSpace(email))).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt = parseTime(created), parseTime(updated)
	return &u, nil
}

func (s *Store) GetUserByID(id string) (*User, error) {
	var u User
	var created, updated string
	err := s.DB.QueryRow(`SELECT id,email,password_hash,name,created_at,updated_at FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt = parseTime(created), parseTime(updated)
	return &u, nil
}

// --- Sessions ---

func (s *Store) CreateSession(tokenHash, userID string, expiresAt time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO sessions(token_hash,user_id,expires_at,created_at) VALUES(?,?,?,?)`,
		tokenHash, userID, expiresAt.UTC().Format(time.RFC3339Nano), nowUTC())
	return err
}

// GetSessionUser returns the user for a valid, unexpired session token hash.
func (s *Store) GetSessionUser(tokenHash string) (*User, error) {
	var userID, expires string
	err := s.DB.QueryRow(`SELECT user_id,expires_at FROM sessions WHERE token_hash=?`, tokenHash).Scan(&userID, &expires)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if exp := parseTime(expires); !exp.IsZero() && time.Now().UTC().After(exp) {
		_, _ = s.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
		return nil, nil
	}
	return s.GetUserByID(userID)
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) DeleteExpiredSessions() (int64, error) {
	res, err := s.DB.Exec(`DELETE FROM sessions WHERE expires_at < ?`, nowUTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Workspaces ---

func (s *Store) CreateWorkspace(w *Workspace) error {
	w.CreatedAt, w.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if w.Plan == "" {
		w.Plan = "free"
	}
	_, err := s.DB.Exec(`INSERT INTO workspaces(id,public_id,name,owner_user_id,plan,stripe_customer_id,stripe_subscription_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		w.ID, w.PublicID, w.Name, w.OwnerUserID, w.Plan, w.StripeCustomerID, w.StripeSubscriptionID, w.CreatedAt.Format(time.RFC3339Nano), w.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func scanWorkspace(row interface{ Scan(...any) error }) (*Workspace, error) {
	var w Workspace
	var created, updated string
	if err := row.Scan(&w.ID, &w.PublicID, &w.Name, &w.OwnerUserID, &w.Plan, &w.StripeCustomerID, &w.StripeSubscriptionID, &created, &updated); err != nil {
		return nil, err
	}
	w.CreatedAt, w.UpdatedAt = parseTime(created), parseTime(updated)
	return &w, nil
}

const workspaceCols = `id,public_id,name,owner_user_id,plan,stripe_customer_id,stripe_subscription_id,created_at,updated_at`

func (s *Store) GetWorkspace(id string) (*Workspace, error) {
	w, err := scanWorkspace(s.DB.QueryRow(`SELECT `+workspaceCols+` FROM workspaces WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return w, err
}

func (s *Store) GetWorkspaceByPublicID(publicID string) (*Workspace, error) {
	w, err := scanWorkspace(s.DB.QueryRow(`SELECT `+workspaceCols+` FROM workspaces WHERE public_id=?`, publicID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return w, err
}

func (s *Store) ListWorkspacesForUser(userID string) ([]*Workspace, error) {
	rows, err := s.DB.Query(`SELECT `+workspaceCols+` FROM workspaces WHERE id IN (SELECT workspace_id FROM workspace_memberships WHERE user_id=?) ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) AddMembership(workspaceID, userID, role string) error {
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO workspace_memberships(workspace_id,user_id,role,created_at) VALUES(?,?,?,?)`, workspaceID, userID, role, nowUTC())
	return err
}

func (s *Store) IsMember(workspaceID, userID string) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM workspace_memberships WHERE workspace_id=? AND user_id=?`, workspaceID, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) UpdateWorkspacePlan(id, plan, stripeCustomerID, stripeSubscriptionID string) error {
	_, err := s.DB.Exec(`UPDATE workspaces SET plan=?, stripe_customer_id=?, stripe_subscription_id=?, updated_at=? WHERE id=?`,
		plan, stripeCustomerID, stripeSubscriptionID, nowUTC(), id)
	return err
}

func (s *Store) SetWorkspacePlan(id, plan string) error {
	w, err := s.GetWorkspace(id)
	if err != nil || w == nil {
		return err
	}
	return s.UpdateWorkspacePlan(id, plan, w.StripeCustomerID, w.StripeSubscriptionID)
}

// --- Workspace API keys ---

// CreateWorkspaceAPIKey stores only the hash of key. Callers must return
// the plain key to the user once; it cannot be recovered later.
func (s *Store) CreateWorkspaceAPIKey(id, workspaceID, name, plainKey string) (*WorkspaceAPIKey, error) {
	prefix := plainKey
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	_, err := s.DB.Exec(`INSERT INTO workspace_api_keys(id,workspace_id,name,key_hash,key_prefix,created_at,revoked) VALUES(?,?,?,?,?,?,0)`,
		id, workspaceID, name, HashToken(plainKey), prefix, nowUTC())
	if err != nil {
		return nil, err
	}
	return &WorkspaceAPIKey{ID: id, WorkspaceID: workspaceID, Name: name, KeyPrefix: prefix, CreatedAt: time.Now().UTC()}, nil
}

func (s *Store) GetWorkspaceByAPIKey(plainKey string) (*Workspace, error) {
	var wsID string
	err := s.DB.QueryRow(`SELECT workspace_id FROM workspace_api_keys WHERE key_hash=? AND revoked=0`, HashToken(plainKey)).Scan(&wsID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, _ = s.DB.Exec(`UPDATE workspace_api_keys SET last_used_at=? WHERE key_hash=?`, nowUTC(), HashToken(plainKey))
	return s.GetWorkspace(wsID)
}

func (s *Store) ListWorkspaceAPIKeys(workspaceID string) ([]*WorkspaceAPIKey, error) {
	rows, err := s.DB.Query(`SELECT id,workspace_id,name,key_prefix,created_at,revoked FROM workspace_api_keys WHERE workspace_id=? ORDER BY created_at ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WorkspaceAPIKey
	for rows.Next() {
		var k WorkspaceAPIKey
		var created string
		var revoked int
		if err := rows.Scan(&k.ID, &k.WorkspaceID, &k.Name, &k.KeyPrefix, &created, &revoked); err != nil {
			return nil, err
		}
		k.CreatedAt = parseTime(created)
		k.Revoked = revoked == 1
		out = append(out, &k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeWorkspaceAPIKey(workspaceID, keyID string) (bool, error) {
	res, err := s.DB.Exec(`UPDATE workspace_api_keys SET revoked=1 WHERE id=? AND workspace_id=?`, keyID, workspaceID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// --- Usage ---

func (s *Store) MonthlyEventCount(workspaceID string) (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COALESCE(event_count,0) FROM usage_monthly WHERE workspace_id=? AND month=?`, workspaceID, currentMonth()).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

func (s *Store) GetUsage(workspaceID string) (*Usage, error) {
	n, err := s.MonthlyEventCount(workspaceID)
	if err != nil {
		return nil, err
	}
	return &Usage{WorkspaceID: workspaceID, Month: currentMonth(), EventCount: n}, nil
}

func (s *Store) incrementUsageTx(tx *sql.Tx, workspaceID string) error {
	if workspaceID == "" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO usage_monthly(workspace_id,month,event_count) VALUES(?,?,1)
		ON CONFLICT(workspace_id,month) DO UPDATE SET event_count=event_count+1`, workspaceID, currentMonth())
	return err
}

// --- Sources ---

func (s *Store) UpsertSource(name, secret string) error {
	return s.UpsertSourceForWorkspace("", name, secret)
}

func (s *Store) UpsertSourceForWorkspace(workspaceID, name, secret string) error {
	_, err := s.DB.Exec(`INSERT INTO sources(workspace_id,name,secret,created_at) VALUES(?,?,?,?)
		ON CONFLICT(workspace_id,name) DO UPDATE SET secret=excluded.secret`, workspaceID, name, secret, nowUTC())
	return err
}

func (s *Store) GetSourceSecret(name string) (string, bool, error) {
	return s.GetSourceSecretForWorkspace("", name)
}

func (s *Store) GetSourceSecretForWorkspace(workspaceID, name string) (string, bool, error) {
	var secret string
	err := s.DB.QueryRow(`SELECT secret FROM sources WHERE workspace_id=? AND name=?`, workspaceID, name).Scan(&secret)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return secret, true, nil
}

func (s *Store) ListSources() ([]string, error) { return s.ListSourcesForWorkspace("") }

func (s *Store) ListSourcesForWorkspace(workspaceID string) ([]string, error) {
	rows, err := s.DB.Query(`SELECT name FROM sources WHERE workspace_id=? ORDER BY name`, workspaceID)
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
	_, err := s.DB.Exec(`INSERT INTO destinations(id,workspace_id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, d.ID, d.WorkspaceID, d.Name, d.SourceFilter, d.URL, d.Method, marshalHeaders(d.Headers), d.TimeoutSeconds, d.MaxRetries, d.BackoffBaseMs, d.BackoffMaxMs, d.Filter, boolToInt(d.Enabled), d.CreatedAt.Format(time.RFC3339Nano), d.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

const destinationCols = `id,workspace_id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at`

func scanDestination(rows interface{ Scan(...any) error }) (*Destination, error) {
	var d Destination
	var headers, created, updated string
	var enabled int
	if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.SourceFilter, &d.URL, &d.Method, &headers, &d.TimeoutSeconds, &d.MaxRetries, &d.BackoffBaseMs, &d.BackoffMaxMs, &d.Filter, &enabled, &created, &updated); err != nil {
		return nil, err
	}
	d.Headers = unmarshalHeaders(headers)
	d.Enabled = enabled == 1
	d.CreatedAt, d.UpdatedAt = parseTime(created), parseTime(updated)
	return &d, nil
}

func (s *Store) GetDestination(id string) (*Destination, error) {
	row := s.DB.QueryRow(`SELECT `+destinationCols+` FROM destinations WHERE id=?`, id)
	d, err := scanDestination(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) GetDestinationForWorkspace(id, workspaceID string) (*Destination, error) {
	row := s.DB.QueryRow(`SELECT `+destinationCols+` FROM destinations WHERE id=? AND workspace_id=?`, id, workspaceID)
	d, err := scanDestination(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) GetDestinationByName(name string) (*Destination, error) {
	return s.GetDestinationByNameForWorkspace("", name)
}

func (s *Store) GetDestinationByNameForWorkspace(workspaceID, name string) (*Destination, error) {
	row := s.DB.QueryRow(`SELECT `+destinationCols+` FROM destinations WHERE workspace_id=? AND name=?`, workspaceID, name)
	d, err := scanDestination(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) ListDestinations() ([]*Destination, error) {
	return s.ListDestinationsForWorkspace("")
}

func (s *Store) ListDestinationsForWorkspace(workspaceID string) ([]*Destination, error) {
	rows, err := s.DB.Query(`SELECT `+destinationCols+` FROM destinations WHERE workspace_id=? ORDER BY name`, workspaceID)
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

func (s *Store) CountDestinationsForWorkspace(workspaceID string) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM destinations WHERE workspace_id=?`, workspaceID).Scan(&n)
	return n, err
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

func (s *Store) DeleteDestinationForWorkspace(id, workspaceID string) (bool, error) {
	res, err := s.DB.Exec(`DELETE FROM destinations WHERE id=? AND workspace_id=?`, id, workspaceID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MatchingDestinations returns enabled destinations whose source filter
// matches source (empty filter = all) and whose substring filter, if set,
// appears in the payload. Legacy wrapper: workspace "" only.
func (s *Store) MatchingDestinations(source string, payload []byte) ([]*Destination, error) {
	return s.MatchingDestinationsForWorkspace("", source, payload)
}

func (s *Store) MatchingDestinationsForWorkspace(workspaceID, source string, payload []byte) ([]*Destination, error) {
	all, err := s.ListDestinationsForWorkspace(workspaceID)
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
// If an idempotency key already exists for this workspace+source, the
// existing event is returned with existed=true and no duplicate
// deliveries are created. The caller must only acknowledge the webhook
// after this returns nil. Usage is incremented in the same transaction
// for non-duplicate events in a non-empty workspace.
func (s *Store) CreateEvent(ev *Event, dests []*Destination) (existed bool, err error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if ev.IdempotencyKey != "" {
		var existing string
		err := tx.QueryRow(`SELECT id FROM events WHERE workspace_id=? AND source=? AND idempotency_key=?`, ev.WorkspaceID, ev.Source, ev.IdempotencyKey).Scan(&existing)
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
	if _, err := tx.Exec(`INSERT INTO events(id,workspace_id,source,payload,headers_json,content_type,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		ev.ID, ev.WorkspaceID, ev.Source, ev.Payload, marshalHeaders(ev.Headers), ev.ContentType, keyVal, now); err != nil {
		return false, err
	}
	ev.CreatedAt = parseTime(now)
	for _, d := range dests {
		delID := "dlv_" + ev.ID[4:] + "_" + d.ID
		if len(delID) > 64 {
			delID = delID[:64]
		}
		if _, err := tx.Exec(`INSERT INTO deliveries(id,workspace_id,event_id,destination_id,status,attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,0,?,?,?)`,
			delID, ev.WorkspaceID, ev.ID, d.ID, "pending", now, now, now); err != nil {
			return false, err
		}
	}
	if err := s.incrementUsageTx(tx, ev.WorkspaceID); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

const eventCols = `id,workspace_id,source,payload,headers_json,content_type,idempotency_key,created_at`

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var ev Event
	var headers, created string
	var key sql.NullString
	if err := row.Scan(&ev.ID, &ev.WorkspaceID, &ev.Source, &ev.Payload, &headers, &ev.ContentType, &key, &created); err != nil {
		return nil, err
	}
	ev.IdempotencyKey = key.String
	ev.Headers = unmarshalHeaders(headers)
	ev.CreatedAt = parseTime(created)
	ev.PayloadText = string(ev.Payload)
	return &ev, nil
}

func (s *Store) GetEvent(id string) (*Event, error) {
	ev, err := scanEvent(s.DB.QueryRow(`SELECT `+eventCols+` FROM events WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ev, err
}

func (s *Store) GetEventForWorkspace(id, workspaceID string) (*Event, error) {
	ev, err := scanEvent(s.DB.QueryRow(`SELECT `+eventCols+` FROM events WHERE id=? AND workspace_id=?`, id, workspaceID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ev, err
}

func (s *Store) ListEvents(limit int, source, status string, search string) ([]*Event, error) {
	return s.ListEventsForWorkspace("", limit, source, status, search)
}

func (s *Store) ListEventsForWorkspace(workspaceID string, limit int, source, status string, search string) ([]*Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT e.id,e.workspace_id,e.source,e.payload,e.headers_json,e.content_type,e.idempotency_key,e.created_at FROM events e`
	var args []any
	conds := []string{"e.workspace_id=?"}
	args = append(args, workspaceID)
	if source != "" {
		conds = append(conds, "e.source=?")
		args = append(args, source)
	}
	if search != "" {
		conds = append(conds, "e.id LIKE ?")
		args = append(args, "%"+search+"%")
	}
	if status != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id=e.id AND d.status=?)")
		args = append(args, status)
	}
	q += " WHERE " + strings.Join(conds, " AND ")
	q += " ORDER BY e.created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// --- Deliveries ---

func scanDelivery(row interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var next, created, updated string
	var claimedAt, claimedBy sql.NullString
	var lastCode sql.NullInt64
	if err := row.Scan(&d.ID, &d.WorkspaceID, &d.EventID, &d.DestinationID, &d.Status, &d.Attempts, &next, &claimedAt, &claimedBy, &lastCode, &d.LastError, &d.LastDurationMs, &d.LastResponse, &created, &updated); err != nil {
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

const deliveryCols = `d.id,d.workspace_id,d.event_id,d.destination_id,d.status,d.attempts,d.next_attempt_at,d.claimed_at,d.claimed_by,d.last_status_code,d.last_error,d.last_duration_ms,d.last_response,d.created_at,d.updated_at`

func (s *Store) GetDelivery(id string) (*Delivery, error) {
	row := s.DB.QueryRow(`SELECT `+deliveryCols+` FROM deliveries d WHERE d.id=?`, id)
	d, err := scanDelivery(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) GetDeliveryForWorkspace(id, workspaceID string) (*Delivery, error) {
	row := s.DB.QueryRow(`SELECT `+deliveryCols+` FROM deliveries d WHERE d.id=? AND d.workspace_id=?`, id, workspaceID)
	d, err := scanDelivery(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Store) ListDeliveries(limit int, status, destinationID string) ([]*Delivery, error) {
	return s.ListDeliveriesForWorkspace("", limit, status, destinationID)
}

func (s *Store) ListDeliveriesForWorkspace(workspaceID string, limit int, status, destinationID string) ([]*Delivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + deliveryCols + `, dest.name, e.source FROM deliveries d JOIN destinations dest ON dest.id=d.destination_id JOIN events e ON e.id=d.event_id`
	var args []any
	conds := []string{"d.workspace_id=?"}
	args = append(args, workspaceID)
	if status != "" {
		conds = append(conds, "d.status=?")
		args = append(args, status)
	}
	if destinationID != "" {
		conds = append(conds, "d.destination_id=?")
		args = append(args, destinationID)
	}
	q += " WHERE " + strings.Join(conds, " AND ")
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
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.EventID, &d.DestinationID, &d.Status, &d.Attempts, &next, &claimedAt, &claimedBy, &lastCode, &d.LastError, &d.LastDurationMs, &d.LastResponse, &created, &updated, &d.DestinationName, &d.Source); err != nil {
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

// ListDeliveriesForEvent returns deliveries for one event, scoped to a workspace.
func (s *Store) ListDeliveriesForEvent(workspaceID, eventID string) ([]*Delivery, error) {
	rows, err := s.DB.Query(`SELECT `+deliveryCols+` FROM deliveries d WHERE d.workspace_id=? AND d.event_id=? ORDER BY d.created_at ASC`, workspaceID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
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
// Workers serve all workspaces; tenancy is enforced at the API layer.
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

// RetryDeliveryForWorkspace is the tenancy-checked variant.
func (s *Store) RetryDeliveryForWorkspace(id, workspaceID string) (bool, error) {
	res, err := s.DB.Exec(`UPDATE deliveries SET status='pending', next_attempt_at=?, claimed_at=NULL, claimed_by=NULL, updated_at=? WHERE id=? AND workspace_id=? AND status IN ('failed','dead','pending')`,
		nowUTC(), nowUTC(), id, workspaceID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReplayEvent creates fresh pending deliveries for an event to all currently
// matching destinations. Callers must pass workspace-scoped destinations.
func (s *Store) ReplayEvent(eventID string, dests []*Destination) (int, error) {
	now := nowUTC()
	count := 0
	for i, d := range dests {
		delID := fmt.Sprintf("rpl_%d_%d_%s", time.Now().UnixNano(), i, d.ID)
		if len(delID) > 64 {
			delID = delID[:64]
		}
		res, err := s.DB.Exec(`INSERT OR IGNORE INTO deliveries(id,workspace_id,event_id,destination_id,status,attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,0,?,?,?)`,
			delID, d.WorkspaceID, eventID, d.ID, "pending", now, now, now)
		if err != nil {
			return count, err
		}
		n, _ := res.RowsAffected()
		count += int(n)
	}
	return count, nil
}

// Stats computes overview counts. Legacy wrapper: workspace "" only.
func (s *Store) Stats() (*Stats, error) { return s.StatsForWorkspace("") }

func (s *Store) StatsForWorkspace(workspaceID string) (*Stats, error) {
	st := &Stats{}
	row := s.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM events WHERE workspace_id=?),
		(SELECT COUNT(*) FROM deliveries WHERE workspace_id=? AND status='delivered'),
		(SELECT COUNT(*) FROM deliveries WHERE workspace_id=? AND status IN ('failed','dead')),
		(SELECT COUNT(*) FROM deliveries WHERE workspace_id=? AND status IN ('pending','in_progress')),
		(SELECT COUNT(*) FROM deliveries WHERE workspace_id=? AND status='dead'),
		(SELECT COALESCE(SUM(CASE WHEN attempts>0 THEN attempts-1 ELSE 0 END),0) FROM deliveries WHERE workspace_id=?),
		(SELECT COUNT(*) FROM deliveries WHERE workspace_id=?)`,
		workspaceID, workspaceID, workspaceID, workspaceID, workspaceID, workspaceID, workspaceID)
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
// Legacy wrapper: workspace "" only. Foreign keys cascade in SQLite only
// with ON DELETE CASCADE, which these tables do not use, so deliveries and
// attempts are deleted explicitly first by DeleteOldEventsForWorkspace.
func (s *Store) DeleteOldEvents(retentionDays int) (int64, error) {
	return s.DeleteOldEventsForWorkspace("", retentionDays)
}

// DeleteOldEventsForWorkspace deletes events, their deliveries and their
// attempts older than retentionDays for one workspace. Returns the number
// of events deleted. 0 or negative retention disables pruning.
func (s *Store) DeleteOldEventsForWorkspace(workspaceID string, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339Nano)
	// Age is judged by the event, not the delivery: a delivery can be
	// created later (replay) for an old event, and deleting the event
	// while its deliveries still exist would fail the foreign key.
	// Attempts first (they reference deliveries), then deliveries, then events.
	if _, err := s.DB.Exec(`DELETE FROM attempts WHERE delivery_id IN (SELECT d.id FROM deliveries d JOIN events e ON e.id=d.event_id WHERE d.workspace_id=? AND e.created_at<?)`, workspaceID, cutoff); err != nil {
		return 0, err
	}
	if _, err := s.DB.Exec(`DELETE FROM deliveries WHERE workspace_id=? AND event_id IN (SELECT id FROM events WHERE workspace_id=? AND created_at<?)`, workspaceID, workspaceID, cutoff); err != nil {
		return 0, err
	}
	res, err := s.DB.Exec(`DELETE FROM events WHERE workspace_id=? AND created_at<?`, workspaceID, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneAllWorkspaces runs retention pruning for every workspace using the
// retention days for that workspace's plan. planRetention maps plan name
// to retention days; unknown plans fall back to defaultDays.
// Returns total events deleted across all workspaces.
func (s *Store) PruneAllWorkspaces(planRetention map[string]int, defaultDays int) (int64, error) {
	rows, err := s.DB.Query(`SELECT id, plan FROM workspaces`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var id, plan string
		if err := rows.Scan(&id, &plan); err != nil {
			return total, err
		}
		days, ok := planRetention[plan]
		if !ok {
			days = defaultDays
		}
		n, err := s.DeleteOldEventsForWorkspace(id, days)
		if err != nil {
			return total, err
		}
		total += n
	}
	// Legacy workspace "" uses the configured default.
	n, err := s.DeleteOldEventsForWorkspace("", defaultDays)
	if err != nil {
		return total, err
	}
	return total + n, rows.Err()
}
