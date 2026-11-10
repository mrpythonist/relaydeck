-- RelayDeck migration 002: hosted multi-tenancy (Phase 1).
-- The application applies this schema automatically on startup (see
-- internal/database Open + migrateHosted). This file is the human-readable
-- reference for operators and reviewers. Do not run it by hand on a live
-- database unless you know what you are doing; the in-code migration
-- rebuilds the three tables below when their old global keys are present.
--
-- New tables: users, workspaces, workspace_memberships,
-- workspace_api_keys, sessions, usage_monthly.
--
-- Changed tables: sources, destinations, events, deliveries gain a
-- workspace_id column (default '' = legacy single-tenant mode).
--   sources:      PRIMARY KEY (workspace_id, name)
--   destinations: UNIQUE (workspace_id, name)
--   events:       UNIQUE (workspace_id, source, idempotency_key)
-- Deliveries gain workspace_id for scoped listing and pruning.
--
-- Pre-Phase-1 databases are migrated in place: rows keep workspace_id ''
-- and are only visible in self-hosted mode. No rows are deleted by this
-- migration. Retention pruning is a separate, per-plan background job.
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
