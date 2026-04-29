package selector

import (
	"context"
	"sync"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type Local struct {
	mu      sync.Mutex
	current map[string]int
}

func NewLocal() *Local {
	return &Local{current: map[string]int{}}
}

func (s *Local) Select(_ context.Context, candidates []domain.Proxy, _ SelectOptions) (domain.Proxy, error) {
	now := time.Now()
	eligibleCandidates := make([]domain.Proxy, 0, len(candidates))
	totalWeight := 0
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if !eligible(candidate, now) {
			continue
		}
		eligibleCandidates = append(eligibleCandidates, candidate)
		totalWeight += candidate.Weight
		seen[candidate.ID] = struct{}{}
	}
	if len(eligibleCandidates) == 0 {
		return domain.Proxy{}, noHealthy(domain.ErrorKindSelectorNoEligible, "local_selectable_no_eligible_proxy", nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for proxyID := range s.current {
		if _, ok := seen[proxyID]; !ok {
			delete(s.current, proxyID)
		}
	}

	var selected domain.Proxy
	selectedScore := 0
	for _, candidate := range eligibleCandidates {
		s.current[candidate.ID] += candidate.Weight
		score := s.current[candidate.ID]
		if selected.ID == "" || score > selectedScore || (score == selectedScore && candidate.ID < selected.ID) {
			selected = candidate
			selectedScore = score
		}
	}
	s.current[selected.ID] -= totalWeight
	return selected, nil
}
