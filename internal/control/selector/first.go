package selector

import (
	"context"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type FirstSelectable struct{}

func (FirstSelectable) Select(_ context.Context, _ string, candidates []domain.Proxy, _ SelectOptions) (domain.Proxy, error) {
	var selected domain.Proxy
	for _, candidate := range candidates {
		if !candidate.Healthy || candidate.Weight <= 0 || candidate.HealthScore <= 0 {
			continue
		}
		if selected.ID == "" || candidate.Weight > selected.Weight || (candidate.Weight == selected.Weight && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	if selected.ID == "" {
		return domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	return selected, nil
}
