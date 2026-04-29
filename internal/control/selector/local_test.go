package selector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func TestLocalSelectorWeightedRoundRobin(t *testing.T) {
	sel := NewLocal()
	candidates := []domain.Proxy{
		{ID: "a", Healthy: true, Weight: 1, HealthScore: 100},
		{ID: "b", Healthy: true, Weight: 3, HealthScore: 100},
	}

	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		proxy, err := sel.Select(context.Background(), candidates, SelectOptions{})
		if err != nil {
			t.Fatalf("Select() error = %v", err)
		}
		counts[proxy.ID]++
	}

	if counts["a"] != 2 || counts["b"] != 6 {
		t.Fatalf("selection counts = %+v, want a=2 b=6", counts)
	}
}

func TestLocalSelectorFiltersIneligibleCandidates(t *testing.T) {
	sel := NewLocal()
	now := time.Now()
	candidates := []domain.Proxy{
		{ID: "unhealthy", Healthy: false, Weight: 100, HealthScore: 100},
		{ID: "zero-weight", Healthy: true, Weight: 0, HealthScore: 100},
		{ID: "zero-score", Healthy: true, Weight: 100, HealthScore: 0},
		{ID: "open-circuit", Healthy: true, Weight: 100, HealthScore: 100, CircuitOpenUntil: now.Add(time.Minute)},
		{ID: "eligible", Healthy: true, Weight: 1, HealthScore: 1},
	}

	proxy, err := sel.Select(context.Background(), candidates, SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if proxy.ID != "eligible" {
		t.Fatalf("Select() = %q, want eligible", proxy.ID)
	}
}

func TestLocalSelectorNoEligibleCandidates(t *testing.T) {
	sel := NewLocal()
	_, err := sel.Select(context.Background(), []domain.Proxy{
		{ID: "open-circuit", Healthy: true, Weight: 1, HealthScore: 1, CircuitOpenUntil: time.Now().Add(time.Minute)},
	}, SelectOptions{})
	if err == nil {
		t.Fatal("Select() error = nil, want no healthy proxy")
	}
}

func TestLocalSelectorConcurrentSelect(t *testing.T) {
	sel := NewLocal()
	candidates := []domain.Proxy{
		{ID: "a", Healthy: true, Weight: 1, HealthScore: 100},
		{ID: "b", Healthy: true, Weight: 1, HealthScore: 100},
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sel.Select(context.Background(), candidates, SelectOptions{}); err != nil {
				t.Errorf("Select() error = %v", err)
			}
		}()
	}
	wg.Wait()
}
