package domain

import "time"

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type Principal struct {
	ID       string   `json:"principal_id"`
	Type     string   `json:"principal_type"`
	TenantID string   `json:"tenant_id"`
	Issuer   string   `json:"issuer,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

type Subject struct {
	Type   string            `json:"subject_type"`
	ID     string            `json:"subject_id"`
	Labels map[string]string `json:"labels,omitempty"`
}

func (s Subject) StableID() string { return s.Type + ":" + s.ID }

type ResourceRef struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Scope  string            `json:"scope,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

func (r ResourceRef) StableID() string { return r.Kind + ":" + r.Scope + ":" + r.ID }

type PolicyRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Hash    string `json:"hash"`
}

type Lease struct {
	ID             string      `json:"lease_id"`
	TenantID       string      `json:"tenant_id"`
	Generation     int64       `json:"lease_generation"`
	Subject        Subject     `json:"subject"`
	ResourceRef    ResourceRef `json:"resource_ref"`
	PolicyRef      PolicyRef   `json:"policy_ref"`
	GatewayURL     string      `json:"gateway_url"`
	Username       string      `json:"username"`
	// Password 是租约的明文凭据，仅在 CreateLease 响应中一次性返回；
	// 持久化层不得写入此字段，scan 时必须保持为空。
	Password string `json:"password,omitempty"`
	// PasswordHash 是 Password 的不可逆哈希，存于 password_hash 列；
	// 校验时使用 subtle.ConstantTimeCompare 比较哈希，绝不返回给客户端。
	PasswordHash   string      `json:"-"`
	ProxyID        string      `json:"proxy_id"`
	ExpiresAt      time.Time   `json:"expires_at"`
	RenewBefore    time.Time   `json:"renew_before"`
	CatalogVersion string      `json:"catalog_version"`
	CandidateSetID string      `json:"candidate_set_id"`
	Revoked        bool        `json:"revoked"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

type Proxy struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"tenant_id"`
	ProviderID  string            `json:"provider_id,omitempty"`
	Endpoint    string            `json:"endpoint"`
	Healthy     bool              `json:"healthy"`
	Weight      int               `json:"weight"`
	Labels      map[string]string `json:"labels,omitempty"`
	LastSeenAt  time.Time         `json:"last_seen_at"`
	FailureHint string            `json:"failure_hint,omitempty"`
}

type Provider struct {
	ID        string            `json:"id"`
	TenantID  string            `json:"tenant_id"`
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Enabled   bool              `json:"enabled"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type Catalog struct {
	TenantID  string    `json:"tenant_id"`
	Version   string    `json:"version"`
	Proxies   []Proxy   `json:"proxies"`
	Generated time.Time `json:"generated_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Policy struct {
	ID           string            `json:"id"`
	TenantID     string            `json:"tenant_id"`
	Version      int64             `json:"version"`
	Name         string            `json:"name"`
	Enabled      bool              `json:"enabled"`
	SubjectType  string            `json:"subject_type,omitempty"`
	ResourceKind string            `json:"resource_kind,omitempty"`
	TTLSeconds   int64             `json:"ttl_seconds"`
	Labels       map[string]string `json:"labels,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

type AuditEvent struct {
	EventID     string            `json:"event_id"`
	TenantID    string            `json:"tenant_id"`
	PrincipalID string            `json:"principal_id,omitempty"`
	Action      string            `json:"action"`
	Resource    string            `json:"resource"`
	OccurredAt  time.Time         `json:"occurred_at"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type UsageEvent struct {
	EventID    string    `json:"event_id"`
	TenantID   string    `json:"tenant_id"`
	LeaseID    string    `json:"lease_id"`
	BytesSent  int64     `json:"bytes_sent"`
	BytesRecv  int64     `json:"bytes_received"`
	OccurredAt time.Time `json:"occurred_at"`
}
