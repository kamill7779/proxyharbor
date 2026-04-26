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
    UNIQUE KEY uk_key_hash (key_hash),
    INDEX idx_tenant (tenant_id, revoked_at),
    INDEX idx_updated_at (updated_at),
    INDEX idx_fp (key_fp),
    CONSTRAINT fk_tk_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS tenant_keys_version (
    id         TINYINT     PRIMARY KEY,
    version    BIGINT      NOT NULL DEFAULT 0,
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO tenant_keys_version (id, version) VALUES (1, 0)
    ON DUPLICATE KEY UPDATE version = version;

-- Rollback: switch PROXYHARBOR_AUTH_MODE back to tenant-keys/legacy first, then optionally DROP TABLE tenant_keys_version; DROP TABLE tenant_keys;
