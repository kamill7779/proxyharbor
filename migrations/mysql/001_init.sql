CREATE TABLE IF NOT EXISTS tenants (
    tenant_id VARCHAR(191) PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS policies (
    policy_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    version BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    subject_type VARCHAR(128),
    resource_kind VARCHAR(128),
    ttl_seconds BIGINT NOT NULL,
    labels_json JSON NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxy_sources (
    source_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    name VARCHAR(255) NOT NULL,
    kind VARCHAR(128) NOT NULL,
    config_json JSON NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxies (
    proxy_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    source_id VARCHAR(191),
    endpoint TEXT NOT NULL,
    healthy BOOLEAN NOT NULL DEFAULT FALSE,
    weight INTEGER NOT NULL DEFAULT 1,
    labels_json JSON NOT NULL,
    last_seen_at TIMESTAMP NULL,
    failure_hint TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxy_health_samples (
    sample_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    proxy_id VARCHAR(191) NOT NULL,
    healthy BOOLEAN NOT NULL,
    latency_ms INTEGER,
    failure_hint TEXT,
    observed_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_catalog_snapshots (
    snapshot_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    version VARCHAR(191) NOT NULL,
    proxies_json JSON NOT NULL,
    generated_at TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_leases (
    lease_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    generation BIGINT NOT NULL,
    subject_json JSON NOT NULL,
    resource_ref_json JSON NOT NULL,
    policy_ref_json JSON NOT NULL,
    gateway_url TEXT NOT NULL,
    username VARCHAR(255) NOT NULL,
    password_hash TEXT NOT NULL,
    proxy_id VARCHAR(191) NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    renew_before TIMESTAMP NOT NULL,
    catalog_version VARCHAR(191) NOT NULL,
    candidate_set_id VARCHAR(191) NOT NULL,
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxy_idempotency_keys (
    idempotency_key VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    stable_subject_id VARCHAR(255) NOT NULL,
    resource_ref VARCHAR(512) NOT NULL,
    request_kind VARCHAR(128) NOT NULL,
    lease_id VARCHAR(191) NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS proxy_revocations (
    revocation_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    lease_id VARCHAR(191) NOT NULL,
    reason TEXT,
    revoked_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_usage_events (
    event_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    lease_id VARCHAR(191) NOT NULL,
    bytes_sent BIGINT NOT NULL DEFAULT 0,
    bytes_received BIGINT NOT NULL DEFAULT 0,
    occurred_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_provider_runs (
    run_id VARCHAR(191) PRIMARY KEY,
    tenant_id VARCHAR(191) NOT NULL,
    source_id VARCHAR(191) NOT NULL,
    status VARCHAR(64) NOT NULL,
    started_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP NULL,
    stats_json JSON NOT NULL,
    error_message TEXT
);

CREATE INDEX idx_policies_tenant ON policies (tenant_id);
CREATE INDEX idx_proxies_tenant_health ON proxies (tenant_id, healthy);
CREATE INDEX idx_proxy_leases_tenant_expiry ON proxy_leases (tenant_id, expires_at);
CREATE INDEX idx_proxy_usage_events_tenant_time ON proxy_usage_events (tenant_id, occurred_at);