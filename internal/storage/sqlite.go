package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	_ "modernc.org/sqlite"
)

const sqliteSchemaVersion = 1

//go:embed migrations/sqlite/init.sql
var sqliteMigrations embed.FS

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(ctx context.Context, path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		path = "./proxyharbor.db"
	}
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLiteStore{db: db}
	if err := store.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.ensureHotPathIndexes(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	return store, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }
func (s *SQLiteStore) DB() *sql.DB  { return s.db }
func (s *SQLiteStore) AdminStore() *SQLiteAdminStore {
	return &SQLiteAdminStore{store: s}
}

func (s *SQLiteStore) CheckDependencies(ctx context.Context) map[string]error {
	pingCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return map[string]error{"sqlite": s.db.PingContext(pingCtx)}
}

func (s *SQLiteStore) ensureSchema(ctx context.Context) error {
	raw, err := sqliteMigrations.ReadFile("migrations/sqlite/init.sql")
	if err != nil {
		return fmt.Errorf("sqlite read init migration: %w", err)
	}
	version, err := RunMigrations(ctx, SQLMigrationDB{DB: s.db}, []Migration{{
		Version: sqliteSchemaVersion,
		Name:    "init",
		SQL:     string(raw),
	}})
	if err != nil {
		return fmt.Errorf("sqlite migrate schema: %w", err)
	}
	if version != sqliteSchemaVersion {
		return fmt.Errorf("sqlite schema version %d does not match expected %d", version, sqliteSchemaVersion)
	}
	return nil
}

func (s *SQLiteStore) ensureHotPathIndexes(ctx context.Context) error {
	for _, statement := range sqliteHotPathIndexStatements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("sqlite ensure hot path index: %w", err)
		}
	}
	return nil
}

var sqliteHotPathIndexStatements = []string{
	`CREATE INDEX IF NOT EXISTS idx_tenant_keys_active_refresh ON tenant_keys(tenant_id, revoked_at, expires_at, updated_at)`,
	`CREATE INDEX IF NOT EXISTS idx_proxies_selectable ON proxies(healthy, weight, health_score, circuit_open_until, proxy_id)`,
	`CREATE INDEX IF NOT EXISTS idx_proxy_leases_renew_cas ON proxy_leases(tenant_id, lease_id, generation, revoked, expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_proxy_leases_proxy_active ON proxy_leases(proxy_id, revoked, expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_proxy_idempotency_tenant_created ON proxy_idempotency_keys(tenant_id, created_at, idempotency_key)`,
	`CREATE INDEX IF NOT EXISTS idx_proxy_usage_events_tenant_order ON proxy_usage_events(tenant_id, occurred_at, event_id)`,
	`CREATE INDEX IF NOT EXISTS idx_proxy_catalog_snapshots_fresh ON proxy_catalog_snapshots(expires_at, generated_at)`,
}

func sqliteDSN(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("sqlite path is empty")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", err
	}
	uriPath := filepath.ToSlash(abs)
	if runtime.GOOS == "windows" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := url.URL{Scheme: "file", Path: uriPath}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous(NORMAL)")
	query.Add("_pragma", "temp_store(MEMORY)")
	dsn.RawQuery = query.Encode()
	return dsn.String(), nil
}

func (s *SQLiteStore) GetLeaseByIdempotency(ctx context.Context, scope IdempotencyScope) (domain.Lease, bool, error) {
	var leaseID string
	err := s.db.QueryRowContext(ctx, `SELECT lease_id FROM proxy_idempotency_keys WHERE idempotency_key = ?`, scope.String()).Scan(&leaseID)
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

func (s *SQLiteStore) CreateLease(ctx context.Context, scope IdempotencyScope, lease domain.Lease) (domain.Lease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Lease{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT lease_id FROM proxy_idempotency_keys WHERE idempotency_key = ?`, scope.String()).Scan(&existing)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return domain.Lease{}, err
		}
		return s.GetLease(ctx, scope.TenantID, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Lease{}, err
	}
	subjectJSON, _ := json.Marshal(lease.Subject)
	resourceJSON, _ := json.Marshal(lease.ResourceRef)
	policyJSON, _ := json.Marshal(lease.PolicyRef)
	lease.Password = ""
	_, err = tx.ExecContext(ctx, `INSERT INTO proxy_leases (lease_id, tenant_id, generation, subject_json, resource_ref_json, policy_ref_json, gateway_url, username, password_hash, proxy_id, expires_at, renew_before, catalog_version, candidate_set_id, revoked, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, lease.ID, lease.TenantID, lease.Generation, string(subjectJSON), string(resourceJSON), string(policyJSON), lease.GatewayURL, lease.Username, lease.PasswordHash, lease.ProxyID, lease.ExpiresAt.UTC(), lease.RenewBefore.UTC(), lease.CatalogVersion, lease.CandidateSetID, lease.Revoked, lease.CreatedAt.UTC(), lease.UpdatedAt.UTC())
	if err != nil {
		return domain.Lease{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO proxy_idempotency_keys (idempotency_key, tenant_id, stable_subject_id, resource_ref, request_kind, lease_id, created_at) VALUES (?,?,?,?,?,?,?)`, scope.String(), scope.TenantID, scope.StableSubjectID, scope.ResourceRef, scope.RequestKind, lease.ID, time.Now().UTC())
	if err != nil {
		return domain.Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Lease{}, err
	}
	return lease, nil
}

func (s *SQLiteStore) GetLease(ctx context.Context, tenantID, id string) (domain.Lease, error) {
	return scanLease(s.db.QueryRowContext(ctx, `SELECT lease_id, tenant_id, generation, subject_json, resource_ref_json, policy_ref_json, gateway_url, username, password_hash, proxy_id, expires_at, renew_before, catalog_version, candidate_set_id, revoked, created_at, updated_at FROM proxy_leases WHERE tenant_id = ? AND lease_id = ?`, tenantID, id))
}

func (s *SQLiteStore) UpdateLease(ctx context.Context, lease domain.Lease) (domain.Lease, error) {
	if lease.Generation <= 1 {
		return domain.Lease{}, domain.ErrStaleLease
	}
	previousGeneration := lease.Generation - 1
	now := time.Now().UTC()
	subjectJSON, _ := json.Marshal(lease.Subject)
	resourceJSON, _ := json.Marshal(lease.ResourceRef)
	policyJSON, _ := json.Marshal(lease.PolicyRef)
	lease.Password = ""
	res, err := s.db.ExecContext(ctx, `UPDATE proxy_leases SET generation=?, subject_json=?, resource_ref_json=?, policy_ref_json=?, gateway_url=?, username=?, password_hash=?, proxy_id=?, expires_at=?, renew_before=?, catalog_version=?, candidate_set_id=?, revoked=?, updated_at=? WHERE tenant_id=? AND lease_id=? AND generation=? AND revoked = FALSE AND expires_at > ?`, lease.Generation, string(subjectJSON), string(resourceJSON), string(policyJSON), lease.GatewayURL, lease.Username, lease.PasswordHash, lease.ProxyID, lease.ExpiresAt.UTC(), lease.RenewBefore.UTC(), lease.CatalogVersion, lease.CandidateSetID, lease.Revoked, now, lease.TenantID, lease.ID, previousGeneration, now)
	if err != nil {
		return domain.Lease{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, getErr := s.GetLease(ctx, lease.TenantID, lease.ID)
		if getErr != nil {
			return domain.Lease{}, domain.ErrNotFound
		}
		if existing.Revoked {
			return domain.Lease{}, domain.ErrLeaseRevoked
		}
		if !now.Before(existing.ExpiresAt) {
			return domain.Lease{}, domain.ErrLeaseExpired
		}
		return domain.Lease{}, domain.ErrStaleLease
	}
	return lease, nil
}

func (s *SQLiteStore) RevokeLease(ctx context.Context, tenantID, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE proxy_leases SET revoked = TRUE, updated_at = ? WHERE tenant_id = ? AND lease_id = ?`, time.Now().UTC(), tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListActiveLeases(ctx context.Context, tenantID string) ([]domain.Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT lease_id, tenant_id, generation, subject_json, resource_ref_json, policy_ref_json, gateway_url, username, password_hash, proxy_id, expires_at, renew_before, catalog_version, candidate_set_id, revoked, created_at, updated_at FROM proxy_leases WHERE tenant_id = ? AND revoked = FALSE AND expires_at > ? ORDER BY lease_id`, tenantID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLeases(rows)
}

func (s *SQLiteStore) DeleteExpiredLeases(ctx context.Context, tenantID string, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM proxy_leases WHERE tenant_id = ? AND expires_at < ?`, tenantID, before.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) ListPolicies(ctx context.Context) ([]domain.Policy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT policy_id, version, name, enabled, COALESCE(subject_type,''), COALESCE(resource_kind,''), ttl_seconds, labels_json, created_at, updated_at FROM policies ORDER BY policy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Policy
	for rows.Next() {
		policy, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, policy)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetPolicy(ctx context.Context, id string) (domain.Policy, error) {
	return scanPolicy(s.db.QueryRowContext(ctx, `SELECT policy_id, version, name, enabled, COALESCE(subject_type,''), COALESCE(resource_kind,''), ttl_seconds, labels_json, created_at, updated_at FROM policies WHERE policy_id = ?`, id))
}

func (s *SQLiteStore) UpsertPolicy(ctx context.Context, policy domain.Policy) (domain.Policy, error) {
	now := time.Now().UTC()
	if policy.ID == "" {
		policy.ID = "policy-" + now.Format("20060102150405.000000000")
	}
	if policy.TTLSeconds == 0 {
		policy.TTLSeconds = 1800
	}
	labels, _ := json.Marshal(policy.Labels)
	_, err := s.db.ExecContext(ctx, `INSERT INTO policies (policy_id, version, name, enabled, subject_type, resource_kind, ttl_seconds, labels_json, created_at, updated_at) VALUES (?, MAX(?,1), ?, ?, NULLIF(?,''), NULLIF(?,''), ?, ?, ?, ?) ON CONFLICT(policy_id) DO UPDATE SET version=version+1, name=excluded.name, enabled=excluded.enabled, subject_type=excluded.subject_type, resource_kind=excluded.resource_kind, ttl_seconds=excluded.ttl_seconds, labels_json=excluded.labels_json, updated_at=excluded.updated_at`, policy.ID, policy.Version, policy.Name, policy.Enabled, policy.SubjectType, policy.ResourceKind, policy.TTLSeconds, string(labels), now, now)
	if err != nil {
		return domain.Policy{}, err
	}
	return s.GetPolicy(ctx, policy.ID)
}

func (s *SQLiteStore) DeletePolicy(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM policies WHERE policy_id = ?`, id)
	return err
}

func (s *SQLiteStore) ListProviders(ctx context.Context) ([]domain.Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider_id, type, name, enabled, labels_json, created_at, updated_at FROM providers ORDER BY provider_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Provider
	for rows.Next() {
		provider, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, provider)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetProvider(ctx context.Context, id string) (domain.Provider, error) {
	return scanProvider(s.db.QueryRowContext(ctx, `SELECT provider_id, type, name, enabled, labels_json, created_at, updated_at FROM providers WHERE provider_id = ?`, id))
}

func (s *SQLiteStore) UpsertProvider(ctx context.Context, provider domain.Provider) (domain.Provider, error) {
	now := time.Now().UTC()
	if provider.ID == "" {
		provider.ID = "provider-" + now.Format("20060102150405.000000000")
	}
	if provider.CreatedAt.IsZero() {
		provider.CreatedAt = now
	}
	provider.UpdatedAt = now
	labels, _ := json.Marshal(provider.Labels)
	_, err := s.db.ExecContext(ctx, `INSERT INTO providers (provider_id,type,name,enabled,labels_json,created_at,updated_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(provider_id) DO UPDATE SET type=excluded.type,name=excluded.name,enabled=excluded.enabled,labels_json=excluded.labels_json,updated_at=excluded.updated_at`, provider.ID, provider.Type, provider.Name, provider.Enabled, string(labels), provider.CreatedAt, provider.UpdatedAt)
	if err != nil {
		return domain.Provider{}, err
	}
	return s.GetProvider(ctx, provider.ID)
}

func (s *SQLiteStore) DeleteProvider(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE provider_id = ?`, id)
	return err
}

func (s *SQLiteStore) GetProxy(ctx context.Context, id string) (domain.Proxy, error) {
	return scanProxy(s.db.QueryRowContext(ctx, proxySelectSQL()+` WHERE proxy_id = ?`, id))
}

func (s *SQLiteStore) UpsertProxy(ctx context.Context, proxy domain.Proxy) (domain.Proxy, error) {
	now := time.Now().UTC()
	if proxy.ID == "" {
		proxy.ID = "proxy-" + now.Format("20060102150405.000000000")
	}
	if proxy.Weight == 0 {
		proxy.Weight = 1
	}
	if proxy.HealthScore == 0 {
		existing, err := s.GetProxy(ctx, proxy.ID)
		if err == nil {
			proxy.HealthScore = existing.HealthScore
			proxy.ConsecutiveFailures = existing.ConsecutiveFailures
			proxy.CircuitOpenUntil = existing.CircuitOpenUntil
			proxy.LatencyEWMAms = existing.LatencyEWMAms
			proxy.LastCheckedAt = existing.LastCheckedAt
			proxy.LastSuccessAt = existing.LastSuccessAt
			proxy.LastFailureAt = existing.LastFailureAt
			proxy.FailureHint = existing.FailureHint
		} else if errors.Is(err, domain.ErrNotFound) {
			proxy.HealthScore = 100
		} else {
			return domain.Proxy{}, err
		}
	}
	if proxy.LastSeenAt.IsZero() {
		proxy.LastSeenAt = now
	}
	labels, _ := json.Marshal(proxy.Labels)
	_, err := s.db.ExecContext(ctx, `INSERT INTO proxies (proxy_id,source_id,endpoint,healthy,weight,health_score,consecutive_failures,circuit_open_until,latency_ewma_ms,last_checked_at,last_success_at,last_failure_at,labels_json,last_seen_at,failure_hint) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(proxy_id) DO UPDATE SET source_id=excluded.source_id,endpoint=excluded.endpoint,healthy=excluded.healthy,weight=excluded.weight,health_score=excluded.health_score,consecutive_failures=excluded.consecutive_failures,circuit_open_until=excluded.circuit_open_until,latency_ewma_ms=excluded.latency_ewma_ms,last_checked_at=excluded.last_checked_at,last_success_at=excluded.last_success_at,last_failure_at=excluded.last_failure_at,labels_json=excluded.labels_json,last_seen_at=excluded.last_seen_at,failure_hint=excluded.failure_hint`, proxy.ID, proxy.ProviderID, proxy.Endpoint, proxy.Healthy, proxy.Weight, proxy.HealthScore, proxy.ConsecutiveFailures, nullTime(proxy.CircuitOpenUntil), nullInt(proxy.LatencyEWMAms), nullTime(proxy.LastCheckedAt), nullTime(proxy.LastSuccessAt), nullTime(proxy.LastFailureAt), string(labels), proxy.LastSeenAt, proxy.FailureHint)
	if err != nil {
		return domain.Proxy{}, err
	}
	return s.GetProxy(ctx, proxy.ID)
}

func (s *SQLiteStore) DeleteProxy(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM proxies WHERE proxy_id = ?`, id)
	return err
}

func (s *SQLiteStore) ChooseHealthyProxy(ctx context.Context) (domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx, proxySelectSQL()+` WHERE healthy = TRUE AND weight > 0 AND health_score > 0 AND (circuit_open_until IS NULL OR circuit_open_until <= ?) ORDER BY weight DESC, proxy_id ASC LIMIT 1`, time.Now().UTC())
	if err != nil {
		return domain.Proxy{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	return scanProxy(rows)
}

func (s *SQLiteStore) ListSelectableProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx, proxySelectSQL()+` WHERE healthy = TRUE AND weight > 0 AND health_score > 0 AND (circuit_open_until IS NULL OR circuit_open_until <= ?) ORDER BY proxy_id`, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProxies(rows)
}

func (s *SQLiteStore) ListCatalogProxies(ctx context.Context) ([]domain.Proxy, error) {
	rows, err := s.db.QueryContext(ctx, proxySelectSQL()+` ORDER BY proxy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProxies(rows)
}

func (s *SQLiteStore) RecordProxyOutcome(ctx context.Context, proxyID string, delta ProxyHealthDelta) error {
	if delta.ObservedAt.IsZero() {
		delta.ObservedAt = time.Now().UTC()
	}
	if delta.Success {
		reward := delta.Reward
		if reward <= 0 {
			reward = 1
		}
		_, err := s.db.ExecContext(ctx, `UPDATE proxies SET healthy=TRUE, consecutive_failures=0, health_score=MIN(100, health_score + ?), latency_ewma_ms=CASE WHEN ? <= 0 THEN latency_ewma_ms WHEN latency_ewma_ms IS NULL THEN ? ELSE CAST(latency_ewma_ms * 0.8 + ? * 0.2 AS INTEGER) END, last_checked_at=?, last_success_at=?, circuit_open_until=CASE WHEN circuit_open_until IS NOT NULL AND circuit_open_until <= ? THEN NULL ELSE circuit_open_until END, failure_hint='' WHERE proxy_id=?`, reward, delta.LatencyMS, delta.LatencyMS, delta.LatencyMS, delta.ObservedAt, delta.ObservedAt, delta.ObservedAt, proxyID)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var failures int
	if err := tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM proxies WHERE proxy_id=?`, proxyID).Scan(&failures); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	nextFailures := failures + 1
	var circuitOpenUntil any
	if nextFailures >= threshold {
		cooldown := time.Duration(baseCooldownSeconds*nextFailures) * time.Second
		maxCooldown := time.Duration(maxCooldownSeconds) * time.Second
		if cooldown > maxCooldown {
			cooldown = maxCooldown
		}
		circuitOpenUntil = delta.ObservedAt.Add(cooldown)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE proxies SET consecutive_failures=?, health_score=MAX(0, health_score - ?), healthy=CASE WHEN ? >= ? THEN FALSE ELSE healthy END, circuit_open_until=COALESCE(?, circuit_open_until), last_checked_at=?, last_failure_at=?, failure_hint=? WHERE proxy_id=?`, nextFailures, penalty, nextFailures, threshold, circuitOpenUntil, delta.ObservedAt, delta.ObservedAt, delta.FailureHint, proxyID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) LatestCatalog(ctx context.Context) (domain.Catalog, error) {
	proxies, err := s.ListCatalogProxies(ctx)
	if err != nil {
		return domain.Catalog{}, err
	}
	now := time.Now().UTC()
	return domain.Catalog{Version: now.Format("20060102150405"), Proxies: proxies, Generated: now, ExpiresAt: now.Add(time.Minute)}, nil
}

func (s *SQLiteStore) SaveCatalogSnapshot(ctx context.Context, catalog domain.Catalog) error {
	raw, _ := json.Marshal(catalog.Proxies)
	_, err := s.db.ExecContext(ctx, `INSERT INTO proxy_catalog_snapshots (snapshot_id,version,proxies_json,generated_at,expires_at) VALUES (?,?,?,?,?) ON CONFLICT(snapshot_id) DO UPDATE SET proxies_json=excluded.proxies_json, generated_at=excluded.generated_at, expires_at=excluded.expires_at`, "global-"+catalog.Version, catalog.Version, string(raw), catalog.Generated.UTC(), catalog.ExpiresAt.UTC())
	return err
}

func proxySelectSQL() string {
	return `SELECT proxy_id, COALESCE(source_id,''), endpoint, healthy, weight, health_score, consecutive_failures, circuit_open_until, latency_ewma_ms, last_checked_at, last_success_at, last_failure_at, labels_json, last_seen_at, COALESCE(failure_hint,'') FROM proxies`
}

func (s *SQLiteStore) AppendAuditEvents(ctx context.Context, events []domain.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO proxy_audit_events (event_id,tenant_id,principal_id,action,resource,metadata_json,occurred_at) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, event := range events {
		if event.EventID == "" {
			continue
		}
		meta, _ := json.Marshal(event.Metadata)
		if _, err := stmt.ExecContext(ctx, event.EventID, event.TenantID, event.PrincipalID, event.Action, event.Resource, string(meta), event.OccurredAt.UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListAuditEvents(ctx context.Context, tenantID string, limit int) ([]domain.AuditEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, tenant_id, principal_id, action, resource, metadata_json, occurred_at FROM proxy_audit_events WHERE tenant_id = ? ORDER BY occurred_at DESC, event_id DESC LIMIT ?`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AuditEvent
	for rows.Next() {
		var event domain.AuditEvent
		var meta string
		if err := rows.Scan(&event.EventID, &event.TenantID, &event.PrincipalID, &event.Action, &event.Resource, &meta, &event.OccurredAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(meta), &event.Metadata)
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) AppendUsageEvents(ctx context.Context, events []domain.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO proxy_usage_events (event_id,tenant_id,lease_id,bytes_sent,bytes_received,occurred_at) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, event := range events {
		if event.EventID == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, event.EventID, event.TenantID, event.LeaseID, event.BytesSent, event.BytesRecv, event.OccurredAt.UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanLeases(rows *sql.Rows) ([]domain.Lease, error) {
	var out []domain.Lease
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, lease)
	}
	return out, rows.Err()
}

func scanProxies(rows *sql.Rows) ([]domain.Proxy, error) {
	var out []domain.Proxy
	for rows.Next() {
		proxy, err := scanProxy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, proxy)
	}
	return out, rows.Err()
}

type SQLiteAdminStore struct {
	store *SQLiteStore
}

func (s *SQLiteAdminStore) GetTenant(ctx context.Context, id string) (domain.Tenant, error) {
	var tenant domain.Tenant
	var status string
	err := s.store.db.QueryRowContext(ctx, `SELECT id, display_name, status, created_at FROM tenants WHERE id=? AND deleted_at IS NULL`, id).Scan(&tenant.ID, &tenant.Name, &status, &tenant.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Tenant{}, domain.ErrTenantNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	tenant.Enabled = status == "active" || status == "enabled"
	return tenant, nil
}

func (s *SQLiteAdminStore) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT id, display_name, status, created_at FROM tenants WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tenant
	for rows.Next() {
		var tenant domain.Tenant
		var status string
		if err := rows.Scan(&tenant.ID, &tenant.Name, &status, &tenant.CreatedAt); err != nil {
			return nil, err
		}
		tenant.Enabled = status == "active" || status == "enabled"
		out = append(out, tenant)
	}
	return out, rows.Err()
}

func (s *SQLiteAdminStore) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	if tenant.ID == "" {
		return domain.ErrBadRequest
	}
	if tenant.CreatedAt.IsZero() {
		tenant.CreatedAt = time.Now().UTC()
	}
	status := "disabled"
	if tenant.Enabled {
		status = "active"
	}
	_, err := s.store.db.ExecContext(ctx, `INSERT INTO tenants (id,display_name,status,created_at,updated_at) VALUES (?,?,?,?,?)`, tenant.ID, tenant.Name, status, tenant.CreatedAt, tenant.CreatedAt)
	return err
}

func (s *SQLiteAdminStore) UpdateTenant(ctx context.Context, id string, displayName *string, status *string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if displayName != nil {
		res, err := tx.ExecContext(ctx, `UPDATE tenants SET display_name=?, updated_at=? WHERE id=? AND deleted_at IS NULL`, *displayName, time.Now().UTC(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrTenantNotFound
		}
	}
	if status != nil {
		switch *status {
		case "active", "enabled", "disabled", "deleted":
		default:
			return domain.ErrBadRequest
		}
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx, `UPDATE tenants SET status=?, updated_at=?, deleted_at=CASE WHEN ?='deleted' THEN ? ELSE deleted_at END WHERE id=? AND deleted_at IS NULL`, *status, now, *status, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrTenantNotFound
		}
		if *status == "disabled" || *status == "deleted" {
			if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys SET revoked_at=COALESCE(revoked_at,?), updated_at=? WHERE tenant_id=?`, now, now, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version=version+1 WHERE id=1`); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *SQLiteAdminStore) SoftDeleteTenant(ctx context.Context, id string) error {
	status := "deleted"
	return s.UpdateTenant(ctx, id, nil, &status)
}

func (s *SQLiteAdminStore) ListTenantKeys(ctx context.Context, tenantID string) ([]auth.TenantKey, error) {
	rows, err := s.store.db.QueryContext(ctx, `SELECT id, tenant_id, key_hash, key_fp, label, purpose, created_by, created_at, expires_at, revoked_at FROM tenant_keys WHERE tenant_id=? ORDER BY created_at DESC, id DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.TenantKey
	for rows.Next() {
		var key auth.TenantKey
		var hash []byte
		var exp, rev sql.NullTime
		if err := rows.Scan(&key.ID, &key.TenantID, &hash, &key.KeyFP, &key.Label, &key.Purpose, &key.CreatedBy, &key.CreatedAt, &exp, &rev); err != nil {
			return nil, err
		}
		key.KeyHash = hex.EncodeToString(hash)
		if exp.Valid {
			key.ExpiresAt = &exp.Time
		}
		if rev.Valid {
			key.RevokedAt = &rev.Time
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func (s *SQLiteAdminStore) CreateTenantKey(ctx context.Context, key auth.TenantKey) error {
	hash, err := hex.DecodeString(key.KeyHash)
	if err != nil {
		hash = []byte(key.KeyHash)
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO tenant_keys (id,tenant_id,key_hash,key_fp,label,purpose,created_by,created_at,updated_at,expires_at,revoked_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, key.ID, key.TenantID, hash, key.KeyFP, key.Label, key.Purpose, key.CreatedBy, key.CreatedAt, time.Now().UTC(), nullTimePtr(key.ExpiresAt), nullTimePtr(key.RevokedAt))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version=version+1 WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteAdminStore) RevokeTenantKey(ctx context.Context, tenantID, keyID string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE tenant_keys SET revoked_at=?, updated_at=? WHERE tenant_id=? AND id=?`, now, now, tenantID, keyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version=version+1 WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteAdminStore) IncrementTenantKeysVersion(ctx context.Context) error {
	return s.store.IncrementTenantKeysVersion(ctx)
}

func (s *SQLiteAdminStore) AppendAuditEvents(ctx context.Context, events []domain.AuditEvent) error {
	return s.store.AppendAuditEvents(ctx, events)
}

func (s *SQLiteStore) GetTenantKeys(ctx context.Context) ([]auth.TenantKeyRow, error) {
	return s.getTenantKeys(ctx, ``)
}

func (s *SQLiteStore) GetTenantKeysSince(ctx context.Context, since time.Time) ([]auth.TenantKeyRow, error) {
	return s.getTenantKeys(ctx, ` AND tk.updated_at > ?`, since)
}

func (s *SQLiteStore) getTenantKeys(ctx context.Context, extra string, args ...any) ([]auth.TenantKeyRow, error) {
	query := `SELECT tk.id, tk.tenant_id, tk.key_hash, tk.key_fp, tk.label, tk.purpose, tk.created_by, tk.created_at, tk.expires_at, tk.revoked_at, tk.last_seen_at FROM tenant_keys tk JOIN tenants t ON t.id=tk.tenant_id WHERE tk.revoked_at IS NULL AND t.deleted_at IS NULL AND t.status IN ('active','enabled')` + extra + ` ORDER BY tk.id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.TenantKeyRow
	for rows.Next() {
		var row auth.TenantKeyRow
		var hash []byte
		var exp, rev, seen sql.NullTime
		if err := rows.Scan(&row.ID, &row.TenantID, &hash, &row.KeyFP, &row.Label, &row.Purpose, &row.CreatedBy, &row.CreatedAt, &exp, &rev, &seen); err != nil {
			return nil, err
		}
		if len(hash) == 32 {
			copy(row.KeyHash[:], hash)
		}
		if exp.Valid {
			row.ExpiresAt = &exp.Time
		}
		if rev.Valid {
			row.RevokedAt = &rev.Time
		}
		if seen.Valid {
			row.LastSeenAt = &seen.Time
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetTenantKeysVersion(ctx context.Context) (int64, error) {
	var version int64
	err := s.db.QueryRowContext(ctx, `SELECT version FROM tenant_keys_version WHERE id=1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

func (s *SQLiteStore) IncrementTenantKeysVersion(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tenant_keys_version SET version=version+1 WHERE id=1`)
	return err
}

func (s *SQLiteStore) CreateTenantKey(ctx context.Context, key auth.TenantKeyRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO tenant_keys (id,tenant_id,key_hash,key_fp,label,purpose,created_by,created_at,updated_at,expires_at,revoked_at,last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, key.ID, key.TenantID, key.KeyHash[:], key.KeyFP, key.Label, key.Purpose, key.CreatedBy, key.CreatedAt, time.Now().UTC(), nullTimePtr(key.ExpiresAt), nullTimePtr(key.RevokedAt), nullTimePtr(key.LastSeenAt))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version=version+1 WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) RevokeTenantKey(ctx context.Context, keyID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE tenant_keys SET revoked_at=?, updated_at=? WHERE id=?`, now, now, keyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version=version+1 WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetTenant(ctx context.Context, tenantID string) (auth.TenantRow, error) {
	var row auth.TenantRow
	var deleted sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, display_name, status, created_by, created_at, updated_at, deleted_at FROM tenants WHERE id=?`, tenantID).Scan(&row.ID, &row.DisplayName, &row.Status, &row.CreatedBy, &row.CreatedAt, &row.UpdatedAt, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.TenantRow{}, domain.ErrTenantNotFound
	}
	if err != nil {
		return auth.TenantRow{}, err
	}
	if deleted.Valid {
		row.DeletedAt = &deleted.Time
	}
	return row, nil
}

func nullTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}
