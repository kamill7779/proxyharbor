package selector

import (
	"context"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

const (
	NameLocal  = "local"
	NameZFair  = "zfair"
	PolicyNone = "none"
)

type SelectOptions struct {
	AffinityKey    string
	AffinityPolicy string
}

type ProxySelector interface {
	Select(ctx context.Context, candidates []domain.Proxy, opts SelectOptions) (domain.Proxy, error)
}

func Name(sel ProxySelector) string {
	switch sel.(type) {
	case *Local:
		return NameLocal
	case *RedisZFair:
		return NameZFair
	default:
		return "custom"
	}
}

func noHealthy(kind domain.ErrorKind, reason string, cause error) error {
	return domain.NewKindedError(domain.ErrNoHealthyProxy, kind, reason, cause)
}

func eligible(candidate domain.Proxy, now time.Time) bool {
	return candidate.Healthy && candidate.Weight > 0 && candidate.HealthScore > 0 && (candidate.CircuitOpenUntil.IsZero() || !candidate.CircuitOpenUntil.After(now))
}
