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
	Select(ctx context.Context, tenantID string, candidates []domain.Proxy, opts SelectOptions) (domain.Proxy, error)
}
