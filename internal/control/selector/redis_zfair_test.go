package selector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func TestRedisZFairBuildSelectionRequestFiltersCandidates(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	sel := &RedisZFair{now: func() time.Time { return now }, quantum: 1000, defaultLatencyMS: 200, maxPromote: 128, maxScan: 64}
	request, err := sel.buildSelectionRequest([]domain.Proxy{
		{ID: "unhealthy", Healthy: false, Weight: 100, HealthScore: 100},
		{ID: "zero-weight", Healthy: true, Weight: 0, HealthScore: 100},
		{ID: "zero-score", Healthy: true, Weight: 100, HealthScore: 0},
		{ID: "open-circuit", Healthy: true, Weight: 100, HealthScore: 100, CircuitOpenUntil: now.Add(time.Minute)},
		{ID: "eligible", Healthy: true, Weight: 2, HealthScore: 75},
	})
	if err != nil {
		t.Fatalf("buildSelectionRequest() error = %v", err)
	}
	if len(request.candidates) != 1 || request.candidates["eligible"].ID != "eligible" {
		t.Fatalf("eligible candidates = %+v, want only eligible", request.candidates)
	}
	if got, want := request.keys, []string{readyKey(), delayedKey(), nodePrefix()}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("keys = %+v, want %+v", got, want)
	}
	if len(request.args) != 6 {
		t.Fatalf("args len = %d, want 6", len(request.args))
	}
	if request.args[0] != now.UnixMilli() || request.args[1] != float64(1000) || request.args[2] != int64(200) || request.args[3] != int64(128) || request.args[4] != int64(64) || request.args[5] != "eligible" {
		t.Fatalf("args = %#v", request.args)
	}
}

func TestRedisZFairSelectFiltersIneligibleCandidates(t *testing.T) {
	server := miniredis.RunT(t)
	sel := newTestRedisZFair(t, server)
	now := sel.now()
	selected, err := sel.Select(context.Background(), []domain.Proxy{
		{ID: "unhealthy", Healthy: false, Weight: 100, HealthScore: 100},
		{ID: "zero-weight", Healthy: true, Weight: 0, HealthScore: 100},
		{ID: "zero-score", Healthy: true, Weight: 100, HealthScore: 0},
		{ID: "open-circuit", Healthy: true, Weight: 100, HealthScore: 100, CircuitOpenUntil: now.Add(time.Minute)},
		{ID: "eligible", Healthy: true, Weight: 2, HealthScore: 100},
	}, SelectOptions{})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selected.ID != "eligible" {
		t.Fatalf("Select() = %q, want eligible", selected.ID)
	}
	for _, id := range []string{"unhealthy", "zero-weight", "zero-score", "open-circuit"} {
		if server.Exists(nodeKey(id)) {
			t.Fatalf("ineligible proxy %q was synced to redis", id)
		}
	}
}

func TestRedisZFairSelectMapsRedisErrors(t *testing.T) {
	server := miniredis.RunT(t)
	sel := newTestRedisZFair(t, server)
	server.Close()
	_, err := sel.Select(context.Background(), []domain.Proxy{{ID: "eligible", Healthy: true, Weight: 1, HealthScore: 100}}, SelectOptions{})
	if err == nil {
		t.Fatal("Select() error = nil, want redis error")
	}
	if !errors.Is(err, domain.ErrNoHealthyProxy) {
		t.Fatalf("Select() error = %v, want public no_healthy_proxy", err)
	}
	if got := domain.ErrorKindOf(err); got != domain.ErrorKindSelectorRedis {
		t.Fatalf("ErrorKindOf() = %q, want selector_redis", got)
	}
	if got := domain.ErrorReason(err); got != "redis_candidate_sync_failed" {
		t.Fatalf("ErrorReason() = %q, want redis_candidate_sync_failed", got)
	}
}

func TestRedisZFairSelectMapsMalformedAndStaleResults(t *testing.T) {
	candidates := map[string]domain.Proxy{"eligible": {ID: "eligible", Healthy: true, Weight: 1, HealthScore: 100}}

	_, err := parseSelectionResult(nil, false, candidates)
	if !strings.Contains(err.Error(), domain.ErrNoHealthyProxy.Error()) || domain.ErrorKindOf(err) != domain.ErrorKindSelectorMalformedResult {
		t.Fatalf("malformed error mapping = %v kind=%q", err, domain.ErrorKindOf(err))
	}

	_, err = parseSelectionResult([]any{"stale"}, true, candidates)
	if !errors.Is(err, domain.ErrNoHealthyProxy) || domain.ErrorKindOf(err) != domain.ErrorKindSelectorStaleResult {
		t.Fatalf("stale error mapping = %v kind=%q", err, domain.ErrorKindOf(err))
	}
}

func TestRedisZFairFallbackSelectsFromRedisNodes(t *testing.T) {
	server := miniredis.RunT(t)
	sel := newTestRedisZFair(t, server)
	candidates := []domain.Proxy{{ID: "a", Healthy: true, Weight: 1, HealthScore: 100}, {ID: "b", Healthy: true, Weight: 2, HealthScore: 100}}
	request, err := sel.buildSelectionRequest(candidates)
	if err != nil {
		t.Fatalf("buildSelectionRequest() error = %v", err)
	}
	for _, candidate := range candidates {
		if err := sel.client.HSet(context.Background(), nodeKey(candidate.ID), map[string]any{"weight": candidate.Weight, "health_score": candidate.HealthScore, "latency_ewma_ms": 200, "circuit_open_until": 0, "virtual_runtime": 0, "next_eligible_at": 0}).Err(); err != nil {
			t.Fatalf("HSet node error = %v", err)
		}
	}
	if err := sel.seedCandidates(context.Background(), request.candidates); err != nil {
		t.Fatalf("seedCandidates() error = %v", err)
	}
	selected, err := sel.evalFallback(context.Background(), request)
	if err != nil {
		t.Fatalf("evalFallback() error = %v", err)
	}
	proxy, err := parseSelectionResult(selected.([]any), true, request.candidates)
	if err != nil {
		t.Fatalf("parseSelectionResult() error = %v", err)
	}
	if proxy.ID == "" {
		t.Fatalf("fallback selected empty proxy: %+v", proxy)
	}
}
func TestRedisZFairConcurrentSelectDoesNotReturnEmpty(t *testing.T) {
	server := miniredis.RunT(t)
	sel := newTestRedisZFair(t, server)
	candidates := make([]domain.Proxy, 10)
	for i := range candidates {
		candidates[i] = domain.Proxy{ID: fmt.Sprintf("proxy-%02d", i), Healthy: true, Weight: i + 1, HealthScore: 100}
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 1200)
	for i := 0; i < 1200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := sel.Select(context.Background(), candidates, SelectOptions{})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("Select() error = %v kind=%q reason=%q", err, domain.ErrorKindOf(err), domain.ErrorReason(err))
		}
	}
}

func TestRedisZFairLuaKeepsWeightedFairness(t *testing.T) {
	server := miniredis.RunT(t)
	sel := newTestRedisZFair(t, server)
	candidates := []domain.Proxy{
		{ID: "one", Healthy: true, Weight: 1, HealthScore: 100},
		{ID: "three", Healthy: true, Weight: 3, HealthScore: 100},
	}
	counts := map[string]int{}
	for i := 0; i < 80; i++ {
		selected, err := sel.Select(context.Background(), candidates, SelectOptions{})
		if err != nil {
			t.Fatalf("Select() #%d error = %v", i, err)
		}
		counts[selected.ID]++
	}
	if counts["one"] < 15 || counts["one"] > 25 || counts["three"] < 55 || counts["three"] > 65 {
		t.Fatalf("counts = %+v, want roughly 1:3", counts)
	}
}

func TestRedisZFairSkipsRedundantCandidateSyncWithinTTL(t *testing.T) {
	server := miniredis.RunT(t)
	sel := newTestRedisZFair(t, server)
	candidates := make([]domain.Proxy, 10)
	for i := range candidates {
		candidates[i] = domain.Proxy{ID: fmt.Sprintf("proxy-%02d", i), Healthy: true, Weight: i + 1, HealthScore: 100}
	}
	if _, err := sel.Select(context.Background(), candidates, SelectOptions{}); err != nil {
		t.Fatalf("initial Select() error = %v", err)
	}
	before := server.CommandCount()
	for i := 0; i < 20; i++ {
		if _, err := sel.Select(context.Background(), candidates, SelectOptions{}); err != nil {
			t.Fatalf("Select() #%d error = %v", i, err)
		}
	}
	if additional := server.CommandCount() - before; additional > 800 {
		t.Fatalf("additional Redis command count = %d, want redundant candidate sync skipped", additional)
	}
}

func newTestRedisZFair(t *testing.T, server *miniredis.Miniredis) *RedisZFair {
	t.Helper()
	sel, err := NewRedisZFair(context.Background(), RedisZFairConfig{Addr: server.Addr(), Quantum: 1000, DefaultLatencyMS: 200, MaxPromote: 128, MaxScan: 128})
	if err != nil {
		t.Fatalf("NewRedisZFair() error = %v", err)
	}
	t.Cleanup(func() { _ = sel.Close() })
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	sel.now = func() time.Time { return now }
	return sel
}
