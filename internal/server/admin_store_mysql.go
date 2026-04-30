package server

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type MySQLAdminStore struct {
	db *sql.DB
}

func NewMySQLAdminStore(db *sql.DB) *MySQLAdminStore {
	return &MySQLAdminStore{db: db}
}

func (s *MySQLAdminStore) GetTenant(ctx context.Context, id string) (domain.Tenant, error) {
	var tenant domain.Tenant
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT id, display_name, status, created_at FROM tenants WHERE id = ? AND deleted_at IS NULL`, id).Scan(&tenant.ID, &tenant.Name, &status, &tenant.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Tenant{}, domain.ErrTenantNotFound
	}
	if err != nil {
		return domain.Tenant{}, err
	}
	tenant.Enabled = status == "active" || status == "enabled"
	return tenant, nil
}

func (s *MySQLAdminStore) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, display_name, status, created_at FROM tenants WHERE deleted_at IS NULL ORDER BY created_at DESC`)
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

func (s *MySQLAdminStore) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	if tenant.ID == "" || tenant.Name == "" {
		return domain.ErrBadRequest
	}
	createdBy := "admin"
	if tenant.CreatedAt.IsZero() {
		tenant.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tenants (id, display_name, status, created_by, created_at, updated_at) VALUES (?, ?, 'active', ?, ?, ?)`, tenant.ID, tenant.Name, createdBy, tenant.CreatedAt.UTC(), tenant.CreatedAt.UTC())
	if err != nil && isMySQLDuplicateKey(err) {
		return domain.ErrIdempotencyConflict
	}
	return err
}

func (s *MySQLAdminStore) UpdateTenant(ctx context.Context, id string, displayName *string, status *string) error {
	if status != nil && !validTenantStatus(*status) {
		return domain.ErrBadRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tenants WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrTenantNotFound
	} else if err != nil {
		return err
	}
	if displayName != nil {
		res, err := tx.ExecContext(ctx, `UPDATE tenants SET display_name = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, *displayName, time.Now().UTC(), id)
		if err != nil {
			return err
		}
		if rows, err := res.RowsAffected(); err == nil && rows == 0 {
			return domain.ErrTenantNotFound
		}
	}
	if status != nil {
		res, err := tx.ExecContext(ctx, `UPDATE tenants SET status = ?, deleted_at = CASE WHEN ? = 'deleted' THEN ? ELSE deleted_at END, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, *status, *status, time.Now().UTC(), time.Now().UTC(), id)
		if err != nil {
			return err
		}
		if rows, err := res.RowsAffected(); err == nil && rows == 0 {
			return domain.ErrTenantNotFound
		}
		if *status == "disabled" || *status == "deleted" {
			if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys SET revoked_at = COALESCE(revoked_at, ?), updated_at = ? WHERE tenant_id = ? AND revoked_at IS NULL`, time.Now().UTC(), time.Now().UTC(), id); err != nil {
				return err
			}
			if err := bumpTenantKeysVersion(ctx, tx); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *MySQLAdminStore) SoftDeleteTenant(ctx context.Context, id string) error {
	status := "deleted"
	return s.UpdateTenant(ctx, id, nil, &status)
}

func (s *MySQLAdminStore) ListTenantKeys(ctx context.Context, tenantID string) ([]auth.TenantKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, key_fp, label, purpose, created_by, created_at, expires_at, revoked_at FROM tenant_keys WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auth.TenantKey
	for rows.Next() {
		var key auth.TenantKey
		var exp, rev sql.NullTime
		if err := rows.Scan(&key.ID, &key.TenantID, &key.KeyFP, &key.Label, &key.Purpose, &key.CreatedBy, &key.CreatedAt, &exp, &rev); err != nil {
			return nil, err
		}
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

func (s *MySQLAdminStore) CreateTenantKey(ctx context.Context, key auth.TenantKey) error {
	hash, err := decodeKeyHash(key.KeyHash)
	if err != nil {
		return domain.ErrBadRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tenants WHERE id = ? AND deleted_at IS NULL AND status IN ('active', 'enabled') FOR UPDATE`, key.TenantID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return domain.ErrTenantNotFound
	} else if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tenant_keys (id, tenant_id, key_hash, key_fp, label, purpose, created_by, created_at, expires_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, key.ID, key.TenantID, hash[:], key.KeyFP, key.Label, key.Purpose, key.CreatedBy, key.CreatedAt.UTC(), nullTime(key.ExpiresAt), nullTime(key.RevokedAt))
	if err != nil {
		if isMySQLDuplicateKey(err) {
			return domain.ErrIdempotencyConflict
		}
		return err
	}
	if err := bumpTenantKeysVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLAdminStore) RevokeTenantKey(ctx context.Context, tenantID, keyID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE tenant_keys SET revoked_at = COALESCE(revoked_at, ?), updated_at = ? WHERE tenant_id = ? AND id = ?`, time.Now().UTC(), time.Now().UTC(), tenantID, keyID)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return domain.ErrNotFound
	}
	if err := bumpTenantKeysVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLAdminStore) IncrementTenantKeysVersion(ctx context.Context) error {
	return bumpTenantKeysVersion(ctx, s.db)
}

func (s *MySQLAdminStore) AppendAuditEvents(ctx context.Context, events []domain.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		metrics.AuditWriteFailures.Add(int64(len(events)))
		slog.Error("admin.audit.begin_tx", "err", err)
		return nil
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT IGNORE INTO proxy_audit_events (event_id, tenant_id, principal_id, action, metadata_json, occurred_at)
		 VALUES (?,?,?,?,?,?)`)
	if err != nil {
		metrics.AuditWriteFailures.Add(int64(len(events)))
		slog.Error("admin.audit.prepare", "err", err)
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
			slog.Warn("admin.audit.write_event", "event_id", e.EventID, "err", err)
			_ = tx.Rollback()
			return nil
		}
	}
	if err := tx.Commit(); err != nil {
		metrics.AuditWriteFailures.Add(int64(len(events)))
		slog.Error("admin.audit.commit", "err", err)
		return nil
	}
	return nil
}

func validTenantStatus(status string) bool {
	switch status {
	case "active", "enabled", "disabled", "deleted":
		return true
	default:
		return false
	}
}

func decodeKeyHash(value string) ([32]byte, error) {
	var out [32]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(out) {
		return out, errors.New("invalid key hash")
	}
	copy(out[:], decoded)
	return out, nil
}

type txExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func bumpTenantKeysVersion(ctx context.Context, tx txExecer) error {
	res, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version = version + 1 WHERE id = 1`)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows == 0 {
		return errors.New("tenant_keys_version_missing")
	}
	return nil
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}

func isMySQLDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
