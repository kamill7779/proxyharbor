SET @schema_name := DATABASE();

SET @column_exists := (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND COLUMN_NAME = 'health_score'
);
SET @ddl := IF(
    @column_exists = 0,
    'ALTER TABLE proxies ADD COLUMN health_score INTEGER NOT NULL DEFAULT 100 AFTER weight',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @column_exists := (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND COLUMN_NAME = 'consecutive_failures'
);
SET @ddl := IF(
    @column_exists = 0,
    'ALTER TABLE proxies ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0 AFTER health_score',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @column_exists := (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND COLUMN_NAME = 'circuit_open_until'
);
SET @ddl := IF(
    @column_exists = 0,
    'ALTER TABLE proxies ADD COLUMN circuit_open_until TIMESTAMP NULL AFTER consecutive_failures',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @column_exists := (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND COLUMN_NAME = 'latency_ewma_ms'
);
SET @ddl := IF(
    @column_exists = 0,
    'ALTER TABLE proxies ADD COLUMN latency_ewma_ms INTEGER NULL AFTER circuit_open_until',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @column_exists := (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND COLUMN_NAME = 'last_checked_at'
);
SET @ddl := IF(
    @column_exists = 0,
    'ALTER TABLE proxies ADD COLUMN last_checked_at TIMESTAMP NULL AFTER latency_ewma_ms',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @column_exists := (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND COLUMN_NAME = 'last_success_at'
);
SET @ddl := IF(
    @column_exists = 0,
    'ALTER TABLE proxies ADD COLUMN last_success_at TIMESTAMP NULL AFTER last_checked_at',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @column_exists := (
    SELECT COUNT(*)
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND COLUMN_NAME = 'last_failure_at'
);
SET @ddl := IF(
    @column_exists = 0,
    'ALTER TABLE proxies ADD COLUMN last_failure_at TIMESTAMP NULL AFTER last_success_at',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

SET @index_exists := (
    SELECT COUNT(*)
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = @schema_name AND TABLE_NAME = 'proxies' AND INDEX_NAME = 'idx_proxies_selectable'
);
SET @ddl := IF(
    @index_exists = 0,
    'CREATE INDEX idx_proxies_selectable ON proxies (tenant_id, healthy, health_score)',
    'SELECT 1'
);
PREPARE stmt FROM @ddl;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;