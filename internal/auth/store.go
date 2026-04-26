package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type TenantKeyRow struct {
	ID         string
	TenantID   string
	KeyHash    [32]byte
	KeyFP      string
	Label      string
	Purpose    string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastSeenAt *time.Time
}

type TenantRow struct {
	ID          string
	DisplayName string
	Status      string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

type KeyStore interface {
	GetTenantKeys(ctx context.Context) ([]TenantKeyRow, error)
	GetTenantKeysSince(ctx context.Context, since time.Time) ([]TenantKeyRow, error)
	GetTenantKeysVersion(ctx context.Context) (int64, error)
	IncrementTenantKeysVersion(ctx context.Context) error
	CreateTenantKey(ctx context.Context, key TenantKeyRow) error
	RevokeTenantKey(ctx context.Context, keyID string) error
	GetTenant(ctx context.Context, tenantID string) (TenantRow, error)
}

type MySQLKeyStore struct {
	db *sql.DB
}

func NewMySQLKeyStore(db *sql.DB) KeyStore {
	return &MySQLKeyStore{db: db}
}

func (s *MySQLKeyStore) GetTenantKeys(ctx context.Context) ([]TenantKeyRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tk.id, tk.tenant_id, tk.key_hash, tk.key_fp, tk.label, tk.purpose, tk.created_by, tk.created_at, tk.expires_at, tk.revoked_at, tk.last_seen_at
		 FROM tenant_keys tk JOIN tenants t ON t.id = tk.tenant_id
		 WHERE tk.revoked_at IS NULL AND t.deleted_at IS NULL AND t.status IN ('active', 'enabled')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantKeyRows(rows)
}

func (s *MySQLKeyStore) GetTenantKeysSince(ctx context.Context, since time.Time) ([]TenantKeyRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tk.id, tk.tenant_id, tk.key_hash, tk.key_fp, tk.label, tk.purpose, tk.created_by, tk.created_at, tk.expires_at, tk.revoked_at, tk.last_seen_at
		 FROM tenant_keys tk JOIN tenants t ON t.id = tk.tenant_id
		 WHERE tk.updated_at > ? AND t.deleted_at IS NULL AND t.status IN ('active', 'enabled')`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantKeyRows(rows)
}

func scanTenantKeyRows(rows *sql.Rows) ([]TenantKeyRow, error) {
	var out []TenantKeyRow
	for rows.Next() {
		var r TenantKeyRow
		var hash []byte
		var exp, rev, seen sql.NullTime
		if err := rows.Scan(&r.ID, &r.TenantID, &hash, &r.KeyFP, &r.Label, &r.Purpose, &r.CreatedBy, &r.CreatedAt, &exp, &rev, &seen); err != nil {
			return nil, err
		}
		if len(hash) == 32 {
			copy(r.KeyHash[:], hash)
		}
		if exp.Valid {
			r.ExpiresAt = &exp.Time
		}
		if rev.Valid {
			r.RevokedAt = &rev.Time
		}
		if seen.Valid {
			r.LastSeenAt = &seen.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *MySQLKeyStore) GetTenantKeysVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx,
		`SELECT version FROM tenant_keys_version WHERE id = 1`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func (s *MySQLKeyStore) IncrementTenantKeysVersion(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tenant_keys_version SET version = version + 1 WHERE id = 1`)
	return err
}

func (s *MySQLKeyStore) CreateTenantKey(ctx context.Context, key TenantKeyRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO tenant_keys (id, tenant_id, key_hash, key_fp, label, purpose, created_by, created_at, expires_at, revoked_at, last_seen_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		key.ID, key.TenantID, key.KeyHash[:], key.KeyFP, key.Label, key.Purpose, key.CreatedBy, key.CreatedAt, nullTime(key.ExpiresAt), nullTime(key.RevokedAt), nullTime(key.LastSeenAt))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version = version + 1 WHERE id = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLKeyStore) RevokeTenantKey(ctx context.Context, keyID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`UPDATE tenant_keys SET revoked_at = ?, updated_at = ? WHERE id = ?`, time.Now().UTC(), time.Now().UTC(), keyID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_keys_version SET version = version + 1 WHERE id = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLKeyStore) GetTenant(ctx context.Context, tenantID string) (TenantRow, error) {
	var r TenantRow
	var del sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, status, created_by, created_at, updated_at, deleted_at FROM tenants WHERE id = ?`, tenantID,
	).Scan(&r.ID, &r.DisplayName, &r.Status, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &del)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantRow{}, errors.New("tenant not found")
	}
	if del.Valid {
		r.DeletedAt = &del.Time
	}
	return r, err
}

func nullTime(t *time.Time) interface{} {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}
