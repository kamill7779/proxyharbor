CREATE TABLE IF NOT EXISTS tenants (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active',
  created_by TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  deleted_at DATETIME NULL
);

CREATE TABLE IF NOT EXISTS tenant_keys (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  key_hash BLOB NOT NULL,
  key_fp TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  purpose TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  expires_at DATETIME NULL,
  revoked_at DATETIME NULL,
  last_seen_at DATETIME NULL,
  FOREIGN KEY (tenant_id) REFERENCES tenants(id)
);
CREATE INDEX IF NOT EXISTS idx_tenant_keys_tenant_id ON tenant_keys(tenant_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenant_keys_key_hash ON tenant_keys(key_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenant_keys_key_fp ON tenant_keys(key_fp);
CREATE INDEX IF NOT EXISTS idx_tenant_keys_active_refresh ON tenant_keys(tenant_id, revoked_at, expires_at, updated_at);

CREATE TABLE IF NOT EXISTS tenant_keys_version (
  id INTEGER PRIMARY KEY,
  version INTEGER NOT NULL
);
INSERT OR IGNORE INTO tenant_keys_version (id, version) VALUES (1, 1);

CREATE TABLE IF NOT EXISTS providers (
  provider_id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  name TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT 1,
  labels_json TEXT NOT NULL DEFAULT '{}',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS proxies (
  proxy_id TEXT PRIMARY KEY,
  source_id TEXT NOT NULL DEFAULT '',
  endpoint TEXT NOT NULL,
  healthy BOOLEAN NOT NULL DEFAULT 1,
  weight INTEGER NOT NULL DEFAULT 1,
  health_score INTEGER NOT NULL DEFAULT 100,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  circuit_open_until DATETIME NULL,
  latency_ewma_ms INTEGER NULL,
  last_checked_at DATETIME NULL,
  last_success_at DATETIME NULL,
  last_failure_at DATETIME NULL,
  labels_json TEXT NOT NULL DEFAULT '{}',
  last_seen_at DATETIME NOT NULL,
  failure_hint TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_proxies_selectable ON proxies(healthy, weight, health_score, circuit_open_until, proxy_id);

CREATE TABLE IF NOT EXISTS policies (
  policy_id TEXT PRIMARY KEY,
  version INTEGER NOT NULL DEFAULT 1,
  name TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT 1,
  subject_type TEXT NULL,
  resource_kind TEXT NULL,
  ttl_seconds INTEGER NOT NULL DEFAULT 1800,
  labels_json TEXT NOT NULL DEFAULT '{}',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
INSERT OR IGNORE INTO policies (policy_id, version, name, enabled, ttl_seconds, labels_json, created_at, updated_at)
VALUES ('default', 1, 'Default Policy', 1, 1800, '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);

CREATE TABLE IF NOT EXISTS proxy_leases (
  lease_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  subject_json TEXT NOT NULL,
  resource_ref_json TEXT NOT NULL,
  policy_ref_json TEXT NOT NULL,
  gateway_url TEXT NOT NULL,
  username TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  proxy_id TEXT NOT NULL,
  expires_at DATETIME NOT NULL,
  renew_before DATETIME NOT NULL,
  catalog_version TEXT NOT NULL,
  candidate_set_id TEXT NOT NULL,
  revoked BOOLEAN NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, lease_id)
);
CREATE INDEX IF NOT EXISTS idx_proxy_leases_active ON proxy_leases(tenant_id, revoked, expires_at);
CREATE INDEX IF NOT EXISTS idx_proxy_leases_renew_cas ON proxy_leases(tenant_id, lease_id, generation, revoked, expires_at);
CREATE INDEX IF NOT EXISTS idx_proxy_leases_proxy_active ON proxy_leases(proxy_id, revoked, expires_at);

CREATE TABLE IF NOT EXISTS proxy_idempotency_keys (
  idempotency_key TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  stable_subject_id TEXT NOT NULL,
  resource_ref TEXT NOT NULL,
  request_kind TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_idempotency_tenant_created ON proxy_idempotency_keys(tenant_id, created_at, idempotency_key);

CREATE TABLE IF NOT EXISTS proxy_usage_events (
  event_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  lease_id TEXT NOT NULL,
  bytes_sent INTEGER NOT NULL,
  bytes_received INTEGER NOT NULL,
  occurred_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_usage_events_tenant_order ON proxy_usage_events(tenant_id, occurred_at, event_id);

CREATE TABLE IF NOT EXISTS proxy_audit_events (
  event_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  principal_id TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  resource TEXT NOT NULL DEFAULT '',
  metadata_json TEXT NOT NULL DEFAULT '{}',
  occurred_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_audit_events_tenant_order ON proxy_audit_events(tenant_id, occurred_at, event_id);

CREATE TABLE IF NOT EXISTS proxy_catalog_snapshots (
  snapshot_id TEXT PRIMARY KEY,
  version TEXT NOT NULL,
  proxies_json TEXT NOT NULL,
  generated_at DATETIME NOT NULL,
  expires_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_catalog_snapshots_fresh ON proxy_catalog_snapshots(expires_at, generated_at);
