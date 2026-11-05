-- RelayDeck initial schema (mirrors the schema embedded in internal/database).
-- The application applies this schema automatically on startup; this file is
-- the human-readable reference for operators and reviewers.
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
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
