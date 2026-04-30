package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

// MySQLStore 是基于 MySQL 8.x 的 Store 实现。
//
// 设计要点：
//   - 表结构与 migrations/mysql/init.sql 保持一致；
//   - JSON 字段统一用 encoding/json 序列化；
//   - 幂等键独立成表 proxy_idempotency_keys，避免 lease 重复插入；
//   - 所有时间使用 UTC，避免跨时区错乱。
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore 打开连接并验证可用。
func NewMySQLStore(ctx context.Context, dsn string, maxOpen, maxIdle int, connMaxAge time.Duration) (*MySQLStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql open: %w", err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(connMaxAge)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql ping: %w", err)
	}
	return &MySQLStore{db: db}, nil
}

// Close 释放连接池。
func (s *MySQLStore) Close() error { return s.db.Close() }

// DB exposes the underlying handle for diagnostics and maintenance tasks.
func (s *MySQLStore) DB() *sql.DB { return s.db }

func (s *MySQLStore) CheckDependencies(ctx context.Context) map[string]error {
	pingCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return map[string]error{"mysql": s.db.PingContext(pingCtx)}
}

// ---------- LeaseStore ----------

func (s *MySQLStore) GetLeaseByIdempotency(ctx context.Context, scope IdempotencyScope) (domain.Lease, bool, error) {
	var leaseID string
	err := s.db.QueryRowContext(ctx,
		`SELECT lease_id FROM proxy_idempotency_keys WHERE idempotency_key = ?`,
		scope.String(),
	).Scan(&leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Lease{}, false, nil
	}
	if err != nil {
		return domain.Lease{}, false, err
	}
	lease, err := s.GetLease(ctx, scope.TenantID, leaseID)
	if err != nil {
		return domain.Lease{}, false, err
	}
	return lease, true, nil
}

func (s *MySQLStore) CreateLease(ctx context.Context, scope IdempotencyScope, lease domain.Lease) (domain.Lease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Lease{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// 幂等检查
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT lease_id FROM proxy_idempotency_keys WHERE idempotency_key = ?`,
		scope.String(),
	).Scan(&existing)
	if err == nil {
		// 已存在，回退到已有 lease
		_ = tx.Commit()
		return s.GetLease(ctx, scope.TenantID, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Lease{}, err
	}

	subjectJSON, _ := json.Marshal(lease.Subject)
	resourceJSON, _ := json.Marshal(lease.ResourceRef)
	policyJSON, _ := json.Marshal(lease.PolicyRef)

	// 任何情况下都不持久化明文密码，调用方必须先填好 PasswordHash。
	lease.Password = ""

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proxy_leases (lease_id, tenant_id, generation, subject_json, resource_ref_json, policy_ref_json,
			gateway_url, username, password_hash, proxy_id, expires_at, renew_before, catalog_version,
			candidate_set_id, revoked, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		lease.ID, lease.TenantID, lease.Generation, subjectJSON, resourceJSON, policyJSON,
		lease.GatewayURL, lease.Username, lease.PasswordHash, lease.ProxyID,
		lease.ExpiresAt.UTC(), lease.RenewBefore.UTC(), lease.CatalogVersion,
		lease.CandidateSetID, lease.Revoked, lease.CreatedAt.UTC(), lease.UpdatedAt.UTC(),
	); err != nil {
		return domain.Lease{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO proxy_idempotency_keys (idempotency_key, tenant_id, stable_subject_id, resource_ref, request_kind, lease_id, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		scope.String(), scope.TenantID, scope.StableSubjectID, scope.ResourceRef, scope.RequestKind, lease.ID, time.Now().UTC(),
	); err != nil {
		if isMySQLDuplicateKey(err) {
			_ = tx.Rollback()
			return s.leaseByIdempotencyAfterConflict(ctx, scope)
		}
		return domain.Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Lease{}, err
	}
	return lease, nil
}

func (s *MySQLStore) leaseByIdempotencyAfterConflict(ctx context.Context, scope IdempotencyScope) (domain.Lease, error) {
	var existing string
	if err := s.db.QueryRowContext(ctx,
		`SELECT lease_id FROM proxy_idempotency_keys WHERE idempotency_key = ?`,
		scope.String(),
	).Scan(&existing); err != nil {
		return domain.Lease{}, err
	}
	return s.GetLease(ctx, scope.TenantID, existing)
}

func isMySQLDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

func (s *MySQLStore) GetLease(ctx context.Context, tenantID, id string) (domain.Lease, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT lease_id, tenant_id, generation, subject_json, resource_ref_json, policy_ref_json,
			gateway_url, username, password_hash, proxy_id, expires_at, renew_before, catalog_version,
			candidate_set_id, revoked, created_at, updated_at
		 FROM proxy_leases WHERE tenant_id = ? AND lease_id = ?`,
		tenantID, id,
	)
	return scanLease(row)
}

func (s *MySQLStore) UpdateLease(ctx context.Context, lease domain.Lease) (domain.Lease, error) {
	if lease.Generation <= 1 {
		return domain.Lease{}, domain.ErrStaleLease
	}
	previousGeneration := lease.Generation - 1
	subjectJSON, _ := json.Marshal(lease.Subject)
	resourceJSON, _ := json.Marshal(lease.ResourceRef)
	policyJSON, _ := json.Marshal(lease.PolicyRef)
	lease.Password = ""
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_leases SET generation=?, subject_json=?, resource_ref_json=?, policy_ref_json=?,
			gateway_url=?, username=?, password_hash=?, proxy_id=?, expires_at=?, renew_before=?,
			catalog_version=?, candidate_set_id=?, revoked=?, updated_at=?
		 WHERE tenant_id=? AND lease_id=? AND generation=? AND revoked = FALSE AND expires_at > ?`,
		lease.Generation, subjectJSON, resourceJSON, policyJSON,
		lease.GatewayURL, lease.Username, lease.PasswordHash, lease.ProxyID,
		lease.ExpiresAt.UTC(), lease.RenewBefore.UTC(), lease.CatalogVersion,
		lease.CandidateSetID, lease.Revoked, time.Now().UTC(),
		lease.TenantID, lease.ID, previousGeneration, time.Now().UTC(),
	)
	if err != nil {
		return domain.Lease{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if existing, err := s.GetLease(ctx, lease.TenantID, lease.ID); err == nil {
			if existing.Revoked {
				return domain.Lease{}, domain.ErrLeaseRevoked
			}
			if !time.Now().UTC().Before(existing.ExpiresAt) {
				return domain.Lease{}, domain.ErrLeaseExpired
			}
			return domain.Lease{}, domain.ErrStaleLease
		}
		return domain.Lease{}, domain.ErrNotFound
	}
	return lease, nil
}

func (s *MySQLStore) RevokeLease(ctx context.Context, tenantID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_leases SET revoked = TRUE, updated_at = ? WHERE tenant_id = ? AND lease_id = ? AND revoked = FALSE`,
		time.Now().UTC(), tenantID, id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetLease(ctx, tenantID, id); err == nil {
			return nil
		}
		return domain.ErrNotFound
	}
	return nil
}

func (s *MySQLStore) ListActiveLeases(ctx context.Context, tenantID string) ([]domain.Lease, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT lease_id, tenant_id, generation, subject_json, resource_ref_json, policy_ref_json,
			gateway_url, username, password_hash, proxy_id, expires_at, renew_before, catalog_version,
			candidate_set_id, revoked, created_at, updated_at
		 FROM proxy_leases WHERE tenant_id = ? AND revoked = FALSE AND expires_at > UTC_TIMESTAMP()
		 ORDER BY lease_id`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Lease, 0)
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *MySQLStore) DeleteExpiredLeases(ctx context.Context, tenantID string, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM proxy_leases WHERE tenant_id = ? AND expires_at < ?`,
		tenantID, before.UTC(),
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *MySQLStore) DeleteExpiredLeasesBatch(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM proxy_leases WHERE expires_at < ? LIMIT ?`,
		before.UTC(), limit,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *MySQLStore) HeartbeatInstance(ctx context.Context, heartbeat InstanceHeartbeat) error {
	startedAt := heartbeat.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	lastSeenAt := heartbeat.LastSeenAt.UTC()
	if lastSeenAt.IsZero() {
		lastSeenAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO instances (instance_id, role, version, config_fingerprint, started_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE role=VALUES(role), version=VALUES(version),
			config_fingerprint=VALUES(config_fingerprint), last_seen_at=VALUES(last_seen_at)`,
		heartbeat.InstanceID, heartbeat.Role, heartbeat.Version, heartbeat.ConfigFingerprint, startedAt, lastSeenAt,
	)
	return err
}

func (s *MySQLStore) TryAcquireLock(ctx context.Context, name, ownerInstanceID string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	leaseUntil := now.Add(ttl)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO cluster_locks (name, owner_instance_id, lease_until, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
			owner_instance_id = IF(lease_until < VALUES(updated_at) OR owner_instance_id = VALUES(owner_instance_id), VALUES(owner_instance_id), owner_instance_id),
			lease_until = IF(lease_until < VALUES(updated_at) OR owner_instance_id = VALUES(owner_instance_id), VALUES(lease_until), lease_until),
			updated_at = IF(lease_until < VALUES(updated_at) OR owner_instance_id = VALUES(owner_instance_id), VALUES(updated_at), updated_at)`,
		name, ownerInstanceID, leaseUntil, now,
	)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	lock, ok, err := s.GetClusterLock(ctx, name)
	if err != nil || !ok {
		return false, err
	}
	return lock.OwnerInstanceID == ownerInstanceID && lock.LeaseUntil.After(now), nil
}

func (s *MySQLStore) GetClusterLock(ctx context.Context, name string) (ClusterLock, bool, error) {
	var lock ClusterLock
	err := s.db.QueryRowContext(ctx,
		`SELECT name, owner_instance_id, lease_until, updated_at FROM cluster_locks WHERE name = ?`,
		name,
	).Scan(&lock.Name, &lock.OwnerInstanceID, &lock.LeaseUntil, &lock.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ClusterLock{}, false, nil
	}
	if err != nil {
		return ClusterLock{}, false, err
	}
	return lock, true, nil
}

// ---------- PolicyStore ----------

func (s *MySQLStore) ListPolicies(ctx context.Context) ([]domain.Policy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT policy_id, version, name, enabled, COALESCE(subject_type,''), COALESCE(resource_kind,''),
			ttl_seconds, labels_json, created_at, updated_at
		 FROM policies ORDER BY policy_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Policy, 0)
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *MySQLStore) GetPolicy(ctx context.Context, id string) (domain.Policy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT policy_id, version, name, enabled, COALESCE(subject_type,''), COALESCE(resource_kind,''),
			ttl_seconds, labels_json, created_at, updated_at
		 FROM policies WHERE policy_id = ?`,
		id,
	)
	return scanPolicy(row)
}

func (s *MySQLStore) UpsertPolicy(ctx context.Context, policy domain.Policy) (domain.Policy, error) {
	now := time.Now().UTC()
	if policy.ID == "" {
		policy.ID = "policy-" + now.Format("20060102150405.000000000")
	}
	if policy.TTLSeconds == 0 {
		policy.TTLSeconds = 1800
	}
	labelsJSON, _ := json.Marshal(policy.Labels)
	// 用 UPSERT，version 在已存在时由 SQL 自增以避免读改写竞态。
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO policies (policy_id, version, name, enabled, subject_type, resource_kind,
			ttl_seconds, labels_json, created_at, updated_at)
		 VALUES (?, GREATEST(?,1), ?, ?, NULLIF(?,''), NULLIF(?,''), ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
			version = version + 1,
			name = VALUES(name),
			enabled = VALUES(enabled),
			subject_type = VALUES(subject_type),
			resource_kind = VALUES(resource_kind),
			ttl_seconds = VALUES(ttl_seconds),
			labels_json = VALUES(labels_json),
			updated_at = VALUES(updated_at)`,
		policy.ID, policy.Version, policy.Name, policy.Enabled,
		policy.SubjectType, policy.ResourceKind, policy.TTLSeconds, labelsJSON, now, now,
	)
	if err != nil {
		return domain.Policy{}, err
	}
	return s.GetPolicy(ctx, policy.ID)
}

func (s *MySQLStore) DeletePolicy(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM policies WHERE policy_id = ?`, id)
	return err
}

// ---------- CatalogStore ----------

func (s *MySQLStore) ListProviders(ctx context.Context) ([]domain.Provider, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_id, kind, name, enabled, config_json, created_at, updated_at
		 FROM proxy_sources ORDER BY source_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Provider, 0)
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *MySQLStore) GetProvider(ctx context.Context, id string) (domain.Provider, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT source_id, kind, name, enabled, config_json, created_at, updated_at
		 FROM proxy_sources WHERE source_id = ?`,
		id,
	)
	return scanProvider(row)
}

func (s *MySQLStore) UpsertProvider(ctx context.Context, p domain.Provider) (domain.Provider, error) {
	now := time.Now().UTC()
	if p.ID == "" {
		p.ID = "provider-" + now.Format("20060102150405.000000000")
	}
	if p.Type == "" {
		p.Type = "static"
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	cfgJSON, _ := json.Marshal(p.Labels)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO proxy_sources (source_id, kind, name, enabled, config_json, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)
		 ON DUPLICATE KEY UPDATE kind=VALUES(kind), name=VALUES(name), enabled=VALUES(enabled),
		   config_json=VALUES(config_json), updated_at=VALUES(updated_at)`,
		p.ID, p.Type, p.Name, p.Enabled, cfgJSON, now, now,
	)
	if err != nil {
		return domain.Provider{}, err
	}
	return s.GetProvider(ctx, p.ID)
}

func (s *MySQLStore) DeleteProvider(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM proxy_sources WHERE source_id = ?`, id)
	return err
}

func (s *MySQLStore) GetProxy(ctx context.Context, id string) (domain.Proxy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT proxy_id, COALESCE(source_id,''), endpoint, healthy, weight,
			health_score, consecutive_failures, circuit_open_until, latency_ewma_ms,
			last_checked_at, last_success_at, last_failure_at, labels_json,
			COALESCE(last_seen_at, UTC_TIMESTAMP()), COALESCE(failure_hint,'')
		 FROM proxies WHERE proxy_id = ?`,
		id,
	)
	return scanProxy(row)
}

func (s *MySQLStore) UpsertProxy(ctx context.Context, p domain.Proxy) (domain.Proxy, error) {
	now := time.Now().UTC()
	if p.ProviderID == "" {
		p.ProviderID = "default"
	}
	if p.ID == "" {
		p.ID = "proxy-" + now.Format("20060102150405.000000000")
	}
	if p.Weight == 0 {
		p.Weight = 1
	}
	if p.HealthScore == 0 && p.ConsecutiveFailures == 0 && p.LastSuccessAt.IsZero() && p.LastFailureAt.IsZero() {
		p.HealthScore = 100
	}
	if p.LastSeenAt.IsZero() {
		p.LastSeenAt = now
	}
	labelsJSON, _ := json.Marshal(p.Labels)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO proxies (proxy_id, source_id, endpoint, healthy, weight, labels_json,
			health_score, consecutive_failures, circuit_open_until, latency_ewma_ms,
			last_checked_at, last_success_at, last_failure_at, last_seen_at, failure_hint, created_at, updated_at)
		 VALUES (?,NULLIF(?,''),?,?,?,?,?,?,?,?,?,?,?,?,NULLIF(?,''),?,?)
		 ON DUPLICATE KEY UPDATE source_id=VALUES(source_id), endpoint=VALUES(endpoint), healthy=VALUES(healthy),
		   weight=VALUES(weight), labels_json=VALUES(labels_json), health_score=VALUES(health_score),
		   consecutive_failures=VALUES(consecutive_failures), circuit_open_until=VALUES(circuit_open_until),
		   latency_ewma_ms=VALUES(latency_ewma_ms), last_checked_at=VALUES(last_checked_at),
		   last_success_at=VALUES(last_success_at), last_failure_at=VALUES(last_failure_at), last_seen_at=VALUES(last_seen_at),
		   failure_hint=VALUES(failure_hint), updated_at=VALUES(updated_at)`,
		p.ID, p.ProviderID, p.Endpoint, p.Healthy, p.Weight, labelsJSON,
		p.HealthScore, p.ConsecutiveFailures, nullTime(p.CircuitOpenUntil), nullInt(p.LatencyEWMAms),
		nullTime(p.LastCheckedAt), nullTime(p.LastSuccessAt), nullTime(p.LastFailureAt), p.LastSeenAt.UTC(), p.FailureHint, now, now,
	)
	if err != nil {
		return domain.Proxy{}, err
	}
	return s.GetProxy(ctx, p.ID)
}

func (s *MySQLStore) DeleteProxy(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM proxies WHERE proxy_id = ?`, id)
	return err
}

func (s *MySQLStore) ChooseHealthyProxy(ctx context.Context) (domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT proxy_id, COALESCE(source_id,''), endpoint, healthy, weight,
			health_score, consecutive_failures, circuit_open_until, latency_ewma_ms,
			last_checked_at, last_success_at, last_failure_at, labels_json,
			COALESCE(last_seen_at, UTC_TIMESTAMP()), COALESCE(failure_hint,'')
		 FROM proxies WHERE healthy = TRUE
		   AND weight > 0 AND health_score > 0
		   AND (circuit_open_until IS NULL OR circuit_open_until <= UTC_TIMESTAMP())
		 ORDER BY weight DESC, proxy_id ASC LIMIT 1`,
	)
	if err != nil {
		return domain.Proxy{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	return scanProxy(rows)
}

func (s *MySQLStore) ListSelectableProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT proxy_id, COALESCE(source_id,''), endpoint, healthy, weight,
			health_score, consecutive_failures, circuit_open_until, latency_ewma_ms,
			last_checked_at, last_success_at, last_failure_at, labels_json,
			COALESCE(last_seen_at, UTC_TIMESTAMP()), COALESCE(failure_hint,'')
		 FROM proxies
		 WHERE healthy = TRUE
		   AND weight > 0 AND health_score > 0
		   AND (circuit_open_until IS NULL OR circuit_open_until <= UTC_TIMESTAMP())
		 ORDER BY proxy_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Proxy, 0)
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *MySQLStore) RecordProxyOutcome(ctx context.Context, proxyID string, delta ProxyHealthDelta) error {
	observedAt := delta.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	if delta.Success {
		reward := delta.Reward
		if reward <= 0 {
			reward = 1
		}
		_, err := s.db.ExecContext(ctx,
			`UPDATE proxies
			 SET consecutive_failures = 0,
			     health_score = LEAST(100, health_score + ?),
			     latency_ewma_ms = CASE
			       WHEN ? <= 0 THEN latency_ewma_ms
			       WHEN latency_ewma_ms IS NULL THEN ?
			       ELSE CAST(latency_ewma_ms * 0.8 + ? * 0.2 AS UNSIGNED)
			     END,
			     last_checked_at = ?,
			     last_success_at = ?,
			     circuit_open_until = CASE
			       WHEN circuit_open_until IS NOT NULL AND circuit_open_until <= ? THEN NULL
			       ELSE circuit_open_until
			     END,
			     updated_at = UTC_TIMESTAMP()
			 WHERE proxy_id = ?`,
			reward, delta.LatencyMS, delta.LatencyMS, delta.LatencyMS, observedAt, observedAt, observedAt, proxyID,
		)
		return err
	}
	penalty := delta.Penalty
	if penalty <= 0 {
		penalty = 10
	}
	threshold := delta.MaxConsecutiveFailure
	if threshold <= 0 {
		threshold = 3
	}
	baseCooldownSeconds := int(delta.BaseCooldown.Seconds())
	if baseCooldownSeconds <= 0 {
		baseCooldownSeconds = 30
	}
	maxCooldownSeconds := int(delta.MaxCooldown.Seconds())
	if maxCooldownSeconds <= 0 {
		maxCooldownSeconds = 300
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE proxies
		 SET consecutive_failures = consecutive_failures + 1,
		     health_score = GREATEST(0, health_score - ?),
		     last_checked_at = ?,
		     last_failure_at = ?,
		     failure_hint = NULLIF(?,''),
		     circuit_open_until = CASE
		       WHEN consecutive_failures + 1 >= ?
		       THEN DATE_ADD(?, INTERVAL LEAST(?, ? * (consecutive_failures + 1)) SECOND)
		       ELSE circuit_open_until
		     END,
		     updated_at = UTC_TIMESTAMP()
		 WHERE proxy_id = ?`,
		penalty, observedAt, observedAt, delta.FailureHint, threshold, observedAt, maxCooldownSeconds, baseCooldownSeconds, proxyID,
	)
	return err
}

func (s *MySQLStore) ListCatalogProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT proxy_id, COALESCE(source_id,''), endpoint, healthy, weight,
			health_score, consecutive_failures, circuit_open_until, latency_ewma_ms,
			last_checked_at, last_success_at, last_failure_at, labels_json,
			COALESCE(last_seen_at, UTC_TIMESTAMP()), COALESCE(failure_hint,'')
		 FROM proxies ORDER BY proxy_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Proxy, 0)
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *MySQLStore) LatestCatalog(ctx context.Context) (domain.Catalog, error) {
	// 优先返回最新快照；若没有快照则基于 proxies 表实时构造。
	row := s.db.QueryRowContext(ctx,
		`SELECT version, proxies_json, generated_at, expires_at FROM proxy_catalog_snapshots
		 ORDER BY generated_at DESC LIMIT 1`,
	)
	var (
		version  string
		raw      []byte
		genAt    time.Time
		expireAt time.Time
	)
	err := row.Scan(&version, &raw, &genAt, &expireAt)
	if err == nil {
		var proxies []domain.Proxy
		_ = json.Unmarshal(raw, &proxies)
		return domain.Catalog{Version: version, Proxies: proxies, Generated: genAt, ExpiresAt: expireAt}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Catalog{}, err
	}
	proxies, err := s.ListCatalogProxies(ctx)
	if err != nil {
		return domain.Catalog{}, err
	}
	now := time.Now().UTC()
	sort.Slice(proxies, func(i, j int) bool { return proxies[i].ID < proxies[j].ID })
	return domain.Catalog{Version: now.Format("20060102150405"), Proxies: proxies, Generated: now, ExpiresAt: now.Add(time.Minute)}, nil
}

func (s *MySQLStore) SaveCatalogSnapshot(ctx context.Context, c domain.Catalog) error {
	raw, err := json.Marshal(c.Proxies)
	if err != nil {
		return err
	}
	id := "global-" + c.Version
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO proxy_catalog_snapshots (snapshot_id, version, proxies_json, generated_at, expires_at)
		 VALUES (?,?,?,?,?)
		 ON DUPLICATE KEY UPDATE proxies_json=VALUES(proxies_json), generated_at=VALUES(generated_at), expires_at=VALUES(expires_at)`,
		id, c.Version, raw, c.Generated.UTC(), c.ExpiresAt.UTC(),
	)
	return err
}

// ---------- AuditStore ----------

func (s *MySQLStore) AppendAuditEvents(ctx context.Context, events []domain.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		metrics.AuditWriteFailures.Add(int64(len(events)))
		slog.Error("audit.begin_tx", "err", err)
		return nil
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT IGNORE INTO proxy_audit_events (event_id, tenant_id, principal_id, action, metadata_json, occurred_at)
		 VALUES (?,?,?,?,?,?)`)
	if err != nil {
		metrics.AuditWriteFailures.Add(int64(len(events)))
		slog.Error("audit.prepare", "err", err)
		return nil
	}
	defer stmt.Close()
	for _, e := range events {
		if e.EventID == "" {
			continue
		}
		meta := map[string]any{"resource": e.Resource}
		for k, v := range e.Metadata {
			meta[k] = v
		}
		metaJSON, _ := json.Marshal(meta)
		if _, err := stmt.ExecContext(ctx, e.EventID, e.TenantID, e.PrincipalID, e.Action, string(metaJSON), e.OccurredAt.UTC()); err != nil {
			metrics.AuditWriteFailures.Inc()
			slog.Warn("audit.write_event", "event_id", e.EventID, "err", err)
			_ = tx.Rollback()
			return nil
		}
	}
	if err := tx.Commit(); err != nil {
		metrics.AuditWriteFailures.Add(int64(len(events)))
		slog.Error("audit.commit", "err", err)
		return nil
	}
	return nil
}

func (s *MySQLStore) ListAuditEvents(ctx context.Context, tenantID string, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, tenant_id, principal_id, action, metadata_json, occurred_at
		 FROM proxy_audit_events WHERE tenant_id = ?
		 ORDER BY occurred_at DESC, event_id DESC LIMIT ?`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []domain.AuditEvent
	for rows.Next() {
		var e domain.AuditEvent
		var metaJSON string
		if err := rows.Scan(&e.EventID, &e.TenantID, &e.PrincipalID, &e.Action, &metaJSON, &e.OccurredAt); err != nil {
			return nil, err
		}
		if metaJSON != "" {
			var meta map[string]any
			if json.Unmarshal([]byte(metaJSON), &meta) == nil {
				if res, ok := meta["resource"].(string); ok {
					e.Resource = res
				}
				e.Metadata = make(map[string]string)
				for k, v := range meta {
					if k != "resource" {
						if sv, ok := v.(string); ok {
							e.Metadata[k] = sv
						}
					}
				}
			}
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

func (s *MySQLStore) AppendUsageEvents(ctx context.Context, events []domain.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT IGNORE INTO proxy_usage_events (event_id, tenant_id, lease_id, bytes_sent, bytes_received, occurred_at)
		 VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		if e.EventID == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, e.EventID, e.TenantID, e.LeaseID, e.BytesSent, e.BytesRecv, e.OccurredAt.UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- 内部 scanner ----------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLease(r rowScanner) (domain.Lease, error) {
	var (
		l                                            domain.Lease
		subjectJSON, resourceJSON, policyJSON        []byte
		expiresAt, renewBefore, createdAt, updatedAt time.Time
	)
	if err := r.Scan(&l.ID, &l.TenantID, &l.Generation, &subjectJSON, &resourceJSON, &policyJSON,
		&l.GatewayURL, &l.Username, &l.PasswordHash, &l.ProxyID, &expiresAt, &renewBefore,
		&l.CatalogVersion, &l.CandidateSetID, &l.Revoked, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Lease{}, domain.ErrNotFound
		}
		return domain.Lease{}, err
	}
	_ = json.Unmarshal(subjectJSON, &l.Subject)
	_ = json.Unmarshal(resourceJSON, &l.ResourceRef)
	_ = json.Unmarshal(policyJSON, &l.PolicyRef)
	l.ExpiresAt = expiresAt
	l.RenewBefore = renewBefore
	l.CreatedAt = createdAt
	l.UpdatedAt = updatedAt
	return l, nil
}

func scanPolicy(r rowScanner) (domain.Policy, error) {
	var (
		p          domain.Policy
		labelsJSON []byte
	)
	if err := r.Scan(&p.ID, &p.Version, &p.Name, &p.Enabled, &p.SubjectType, &p.ResourceKind,
		&p.TTLSeconds, &labelsJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Policy{}, domain.ErrNotFound
		}
		return domain.Policy{}, err
	}
	_ = json.Unmarshal(labelsJSON, &p.Labels)
	return p, nil
}

func scanProvider(r rowScanner) (domain.Provider, error) {
	var (
		p          domain.Provider
		configJSON []byte
	)
	if err := r.Scan(&p.ID, &p.Type, &p.Name, &p.Enabled, &configJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Provider{}, domain.ErrNotFound
		}
		return domain.Provider{}, err
	}
	_ = json.Unmarshal(configJSON, &p.Labels)
	return p, nil
}

func scanProxy(r rowScanner) (domain.Proxy, error) {
	var (
		p                domain.Proxy
		labelsJSON       []byte
		circuitOpenUntil sql.NullTime
		latencyEWMAms    sql.NullInt64
		lastCheckedAt    sql.NullTime
		lastSuccessAt    sql.NullTime
		lastFailureAt    sql.NullTime
	)
	if err := r.Scan(&p.ID, &p.ProviderID, &p.Endpoint, &p.Healthy, &p.Weight,
		&p.HealthScore, &p.ConsecutiveFailures, &circuitOpenUntil, &latencyEWMAms,
		&lastCheckedAt, &lastSuccessAt, &lastFailureAt, &labelsJSON, &p.LastSeenAt, &p.FailureHint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Proxy{}, domain.ErrNotFound
		}
		return domain.Proxy{}, err
	}
	if circuitOpenUntil.Valid {
		p.CircuitOpenUntil = circuitOpenUntil.Time
	}
	if latencyEWMAms.Valid {
		p.LatencyEWMAms = int(latencyEWMAms.Int64)
	}
	if lastCheckedAt.Valid {
		p.LastCheckedAt = lastCheckedAt.Time
	}
	if lastSuccessAt.Valid {
		p.LastSuccessAt = lastSuccessAt.Time
	}
	if lastFailureAt.Valid {
		p.LastFailureAt = lastFailureAt.Time
	}
	_ = json.Unmarshal(labelsJSON, &p.Labels)
	return p, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullInt(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}
