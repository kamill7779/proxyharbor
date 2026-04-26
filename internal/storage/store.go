package storage

import (
	"context"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type IdempotencyScope struct {
	TenantID        string
	StableSubjectID string
	ResourceRef     string
	RequestKind     string
	Key             string
}

func (s IdempotencyScope) String() string {
	return s.TenantID + "|" + s.StableSubjectID + "|" + s.ResourceRef + "|" + s.RequestKind + "|" + s.Key
}

type Store interface {
	LeaseStore
	PolicyStore
	CatalogStore
	AuditStore
}

type DependencyChecker interface {
	CheckDependencies(context.Context) map[string]error
}

type LeaseStore interface {
	GetLeaseByIdempotency(context.Context, IdempotencyScope) (domain.Lease, bool, error)
	CreateLease(context.Context, IdempotencyScope, domain.Lease) (domain.Lease, error)
	GetLease(context.Context, string, string) (domain.Lease, error)
	UpdateLease(context.Context, domain.Lease) (domain.Lease, error)
	RevokeLease(context.Context, string, string) error
	ListActiveLeases(context.Context, string) ([]domain.Lease, error)
	DeleteExpiredLeases(context.Context, string, time.Time) (int, error)
}

type PolicyStore interface {
	ListPolicies(context.Context, string) ([]domain.Policy, error)
	GetPolicy(context.Context, string, string) (domain.Policy, error)
	UpsertPolicy(context.Context, domain.Policy) (domain.Policy, error)
	DeletePolicy(context.Context, string, string) error
}

type CatalogStore interface {
	ListProviders(context.Context, string) ([]domain.Provider, error)
	GetProvider(context.Context, string, string) (domain.Provider, error)
	UpsertProvider(context.Context, domain.Provider) (domain.Provider, error)
	DeleteProvider(context.Context, string, string) error
	ListSelectableProxies(context.Context, string) ([]domain.Proxy, error)
	RecordProxyOutcome(context.Context, string, string, ProxyHealthDelta) error
	ChooseHealthyProxy(context.Context, string) (domain.Proxy, error)
	LatestCatalog(context.Context, string) (domain.Catalog, error)
	GetProxy(context.Context, string, string) (domain.Proxy, error)
	UpsertProxy(context.Context, domain.Proxy) (domain.Proxy, error)
	DeleteProxy(context.Context, string, string) error
	SaveCatalogSnapshot(context.Context, domain.Catalog) error
	ListCatalogProxies(context.Context, string) ([]domain.Proxy, error)
}

type ProxyHealthDelta struct {
	Success               bool
	FailureKind           string
	FailureHint           string
	Penalty               int
	Reward                int
	LatencyMS             int
	MaxConsecutiveFailure int
	BaseCooldown          time.Duration
	MaxCooldown           time.Duration
	ObservedAt            time.Time
}

type AuditStore interface {
	AppendAuditEvents(context.Context, []domain.AuditEvent) error
	ListAuditEvents(context.Context, string, int) ([]domain.AuditEvent, error)
	AppendUsageEvents(context.Context, []domain.UsageEvent) error
}
