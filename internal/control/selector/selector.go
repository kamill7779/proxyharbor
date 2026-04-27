package selector

import (
	"context"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

const (
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

func noHealthy(kind domain.ErrorKind, reason string, cause error) error {
	return domain.NewKindedError(domain.ErrNoHealthyProxy, kind, reason, cause)
}
