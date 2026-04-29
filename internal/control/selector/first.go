package selector

import (
	"context"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type FirstSelectable struct{}

func (FirstSelectable) Select(_ context.Context, candidates []domain.Proxy, _ SelectOptions) (domain.Proxy, error) {
	var selected domain.Proxy
	now := time.Now()
	for _, candidate := range candidates {
		if !eligible(candidate, now) {
			continue
		}
		if selected.ID == "" || candidate.Weight > selected.Weight || (candidate.Weight == selected.Weight && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	if selected.ID == "" {
		return domain.Proxy{}, noHealthy(domain.ErrorKindSelectorNoEligible, "first_selectable_no_eligible_proxy", nil)
	}
	return selected, nil
}
