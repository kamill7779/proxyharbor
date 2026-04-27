CREATE TABLE IF NOT EXISTS tenants (
    id           VARCHAR(64)  PRIMARY KEY,
    display_name VARCHAR(128) NOT NULL,
    status       VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_by   VARCHAR(64)  NOT NULL DEFAULT 'system',
    created_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at   DATETIME(3)  NULL,
    INDEX idx_tenants_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS tenant_keys (
    id           VARCHAR(36)  PRIMARY KEY,
    tenant_id    VARCHAR(64)  NOT NULL,
    key_hash     BINARY(32)   NOT NULL,
    key_fp       CHAR(8)      NOT NULL,
    label        VARCHAR(128) NOT NULL DEFAULT '',
    purpose      VARCHAR(32)  NOT NULL DEFAULT 'general',
    created_by   VARCHAR(64)  NOT NULL,
    created_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    expires_at   DATETIME(3)  NULL,
    revoked_at   DATETIME(3)  NULL,
    last_seen_at DATETIME(3)  NULL,
    UNIQUE KEY uk_tenant_keys_hash (key_hash),
    INDEX idx_tenant_keys_tenant (tenant_id, revoked_at),
    INDEX idx_tenant_keys_updated_at (updated_at),
    CONSTRAINT fk_tenant_keys_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS tenant_keys_version (
    id         TINYINT     PRIMARY KEY,
    version    BIGINT      NOT NULL DEFAULT 0,
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxy_sources (
    source_id   VARCHAR(191) PRIMARY KEY,
    kind        VARCHAR(128) NOT NULL DEFAULT 'static',
    name        VARCHAR(255) NOT NULL,
    enabled     BOOLEAN      NOT NULL DEFAULT TRUE,
    config_json JSON         NOT NULL,
    created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxies (
    proxy_id             VARCHAR(191) PRIMARY KEY,
    source_id            VARCHAR(191) NULL,
    endpoint             TEXT         NOT NULL,
    healthy              BOOLEAN      NOT NULL DEFAULT FALSE,
    weight               INTEGER      NOT NULL DEFAULT 1,
    labels_json          JSON         NOT NULL,
    health_score         INTEGER      NOT NULL DEFAULT 100,
    consecutive_failures INTEGER      NOT NULL DEFAULT 0,
    circuit_open_until   DATETIME(3)  NULL,
    latency_ewma_ms      INTEGER      NULL,
    last_checked_at      DATETIME(3)  NULL,
    last_success_at      DATETIME(3)  NULL,
    last_failure_at      DATETIME(3)  NULL,
    last_seen_at         DATETIME(3)  NULL,
    failure_hint         TEXT         NULL,
    created_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    INDEX idx_proxies_selectable (healthy, health_score, weight),
    INDEX idx_proxies_source (source_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS policies (
    policy_id     VARCHAR(191) PRIMARY KEY,
    version       BIGINT       NOT NULL DEFAULT 1,
    name          VARCHAR(255) NOT NULL,
    enabled       BOOLEAN      NOT NULL DEFAULT TRUE,
    subject_type  VARCHAR(128) NULL,
    resource_kind VARCHAR(128) NULL,
    ttl_seconds   BIGINT       NOT NULL DEFAULT 1800,
    labels_json   JSON         NOT NULL,
    created_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxy_leases (
    lease_id          VARCHAR(191) PRIMARY KEY,
    tenant_id         VARCHAR(191) NOT NULL,
    generation        BIGINT       NOT NULL,
    subject_json      JSON         NOT NULL,
    resource_ref_json JSON         NOT NULL,
    policy_ref_json   JSON         NOT NULL,
    gateway_url       TEXT         NOT NULL,
    username          VARCHAR(255) NOT NULL,
    password_hash     TEXT         NOT NULL,
    proxy_id          VARCHAR(191) NOT NULL,
    expires_at        DATETIME(3)  NOT NULL,
    renew_before      DATETIME(3)  NOT NULL,
    catalog_version   VARCHAR(191) NOT NULL,
    candidate_set_id  VARCHAR(191) NOT NULL,
    revoked           BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    INDEX idx_proxy_leases_tenant_expiry (tenant_id, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxy_idempotency_keys (
    idempotency_key   VARCHAR(191) PRIMARY KEY,
    tenant_id         VARCHAR(191) NOT NULL,
    stable_subject_id VARCHAR(255) NOT NULL,
    resource_ref      VARCHAR(512) NOT NULL,
    request_kind      VARCHAR(128) NOT NULL,
    lease_id          VARCHAR(191) NOT NULL,
    created_at        DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxy_catalog_snapshots (
    snapshot_id  VARCHAR(191) PRIMARY KEY,
    version      VARCHAR(191) NOT NULL,
    proxies_json JSON         NOT NULL,
    generated_at DATETIME(3)  NOT NULL,
    expires_at   DATETIME(3)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxy_usage_events (
    event_id       VARCHAR(191) PRIMARY KEY,
    tenant_id      VARCHAR(191) NOT NULL,
    lease_id       VARCHAR(191) NOT NULL,
    bytes_sent     BIGINT       NOT NULL DEFAULT 0,
    bytes_received BIGINT       NOT NULL DEFAULT 0,
    occurred_at    DATETIME(3)  NOT NULL,
    INDEX idx_proxy_usage_events_tenant_time (tenant_id, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxy_audit_events (
    event_id      VARCHAR(191) PRIMARY KEY,
    tenant_id     VARCHAR(191) NOT NULL,
    principal_id  VARCHAR(191) NOT NULL DEFAULT '',
    action        VARCHAR(128) NOT NULL,
    metadata_json JSON         NOT NULL,
    occurred_at   DATETIME(3)  NOT NULL,
    INDEX idx_proxy_audit_events_tenant_time (tenant_id, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS proxy_health_samples (
    sample_id    VARCHAR(191) PRIMARY KEY,
    proxy_id     VARCHAR(191) NOT NULL,
    healthy      BOOLEAN      NOT NULL,
    latency_ms   INTEGER      NULL,
    failure_hint TEXT         NULL,
    observed_at  DATETIME(3)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO tenant_keys_version (id, version) VALUES (1, 0)
    ON DUPLICATE KEY UPDATE version = version;

INSERT INTO proxy_sources (source_id, kind, name, enabled, config_json)
VALUES ('default', 'static', 'Default provider', TRUE, JSON_OBJECT())
ON DUPLICATE KEY UPDATE enabled = TRUE, updated_at = CURRENT_TIMESTAMP(3);

INSERT INTO policies (policy_id, version, name, enabled, subject_type, resource_kind, ttl_seconds, labels_json)
VALUES ('default', 1, 'Default allow policy', TRUE, NULL, NULL, 1800, JSON_OBJECT())
ON DUPLICATE KEY UPDATE enabled = TRUE, updated_at = CURRENT_TIMESTAMP(3);
