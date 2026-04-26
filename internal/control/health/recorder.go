package health

import (
	"context"
	"sync"
	"time"

	"github.com/kamill7779/proxyharbor/internal/storage"
)

type HealthRecorder interface {
	RecordProxyResult(ctx context.Context, tenantID, proxyID string, result ProxyHealthResult)
}

type ProxyHealthResult struct {
	Success   bool
	Kind      FailureKind
	LatencyMS int
	Hint      string
}

type OutcomeStore interface {
	RecordProxyOutcome(ctx context.Context, tenantID, proxyID string, delta storage.ProxyHealthDelta) error
}

type RecorderOptions struct {
	BufferSize    int
	FlushInterval time.Duration
	Policy        ScoringPolicy
}

type CoalescingRecorder struct {
	store         OutcomeStore
	policy        ScoringPolicy
	flushInterval time.Duration

	mu     sync.Mutex
	events []proxyEvent
	closed bool
	done   chan struct{}
}

type proxyEvent struct {
	tenantID string
	proxyID  string
	result   ProxyHealthResult
}

type deltaKey struct {
	tenantID string
	proxyID  string
}

type proxyBucket struct {
	successCount int
	failureKinds []FailureKind
	latencySumMS int
	latencyCount int
	lastHint     string
}

func NewCoalescingRecorder(store OutcomeStore, options RecorderOptions) *CoalescingRecorder {
	bufferSize := options.BufferSize
	if bufferSize <= 0 {
		bufferSize = 1024
	}
	flushInterval := options.FlushInterval
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}
	policy := options.Policy
	if policy.SuccessReward == 0 || policy.FailurePenalty == nil {
		policy = DefaultScoringPolicy()
	}
	recorder := &CoalescingRecorder{
		store:         store,
		policy:        policy,
		flushInterval: flushInterval,
		events:        make([]proxyEvent, 0, bufferSize),
		done:          make(chan struct{}),
	}
	go recorder.run()
	return recorder
}

func (r *CoalescingRecorder) RecordProxyResult(_ context.Context, tenantID, proxyID string, result ProxyHealthResult) {
	if r == nil || tenantID == "" || proxyID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if len(r.events) == cap(r.events) {
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = proxyEvent{tenantID: tenantID, proxyID: proxyID, result: result}
		return
	}
	r.events = append(r.events, proxyEvent{tenantID: tenantID, proxyID: proxyID, result: result})
}

func (r *CoalescingRecorder) Flush(ctx context.Context) {
	if r == nil || r.store == nil {
		return
	}
	events := r.drain()
	if len(events) == 0 {
		return
	}
	for key, bucket := range r.coalesce(events) {
		if bucket.successCount > 0 {
			latencyMS := 0
			if bucket.latencyCount > 0 {
				latencyMS = bucket.latencySumMS / bucket.latencyCount
			}
			_ = r.store.RecordProxyOutcome(ctx, key.tenantID, key.proxyID, storage.ProxyHealthDelta{
				Success:    true,
				Reward:     r.policy.SuccessReward * bucket.successCount,
				LatencyMS:  latencyMS,
				ObservedAt: time.Now().UTC(),
			})
		}
		for _, kind := range bucket.failureKinds {
			penalty := r.policy.FailurePenalty[kind]
			if penalty <= 0 {
				penalty = r.policy.FailurePenalty[FailureUnknown]
			}
			_ = r.store.RecordProxyOutcome(ctx, key.tenantID, key.proxyID, storage.ProxyHealthDelta{
				Success:               false,
				FailureKind:           kind.String(),
				FailureHint:           bucket.lastHint,
				Penalty:               penalty,
				MaxConsecutiveFailure: r.policy.CircuitOpenThreshold,
				BaseCooldown:          r.policy.CircuitBaseCooldown,
				MaxCooldown:           r.policy.CircuitMaxCooldown,
				ObservedAt:            time.Now().UTC(),
			})
		}
	}
}

func (r *CoalescingRecorder) Close(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	r.mu.Unlock()
	r.Flush(ctx)
}

func (r *CoalescingRecorder) run() {
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.Flush(context.Background())
		case <-r.done:
			return
		}
	}
}

func (r *CoalescingRecorder) drain() []proxyEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	events := append([]proxyEvent(nil), r.events...)
	r.events = r.events[:0]
	return events
}

func (r *CoalescingRecorder) coalesce(events []proxyEvent) map[deltaKey]proxyBucket {
	buckets := make(map[deltaKey]proxyBucket)
	for _, event := range events {
		key := deltaKey{tenantID: event.tenantID, proxyID: event.proxyID}
		bucket := buckets[key]
		if event.result.Success {
			bucket.successCount++
			if event.result.LatencyMS > 0 {
				bucket.latencySumMS += event.result.LatencyMS
				bucket.latencyCount++
			}
		} else {
			bucket.failureKinds = append(bucket.failureKinds, event.result.Kind)
		}
		if event.result.Hint != "" {
			bucket.lastHint = event.result.Hint
		}
		buckets[key] = bucket
	}
	return buckets
}
