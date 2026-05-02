package health

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/kamill7779/proxyharbor/internal/storage"
)

type HealthRecorder interface {
	RecordProxyResult(ctx context.Context, proxyID string, result ProxyHealthResult)
}

type ProxyHealthResult struct {
	Success   bool
	Kind      FailureKind
	LatencyMS int
	Hint      string
}

type OutcomeStore interface {
	RecordProxyOutcome(ctx context.Context, proxyID string, delta storage.ProxyHealthDelta) error
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

	mu      sync.Mutex
	events  []proxyEvent
	closed  bool
	done    chan struct{}
	stopped chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

type proxyEvent struct {
	proxyID string
	result  ProxyHealthResult
}

type deltaKey struct {
	proxyID string
}

type proxyBucket struct {
	successCount int
	latencySumMS int
	latencyCount int
	hasFailure   bool
	failureKind  FailureKind
	failureHint  string
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
	runCtx, cancel := context.WithCancel(context.Background())
	recorder := &CoalescingRecorder{
		store:         store,
		policy:        policy,
		flushInterval: flushInterval,
		events:        make([]proxyEvent, 0, bufferSize),
		done:          make(chan struct{}),
		stopped:       make(chan struct{}),
		ctx:           runCtx,
		cancel:        cancel,
	}
	go recorder.run()
	return recorder
}

func (r *CoalescingRecorder) RecordProxyResult(_ context.Context, proxyID string, result ProxyHealthResult) {
	if r == nil || proxyID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if len(r.events) == cap(r.events) {
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = proxyEvent{proxyID: proxyID, result: result}
		return
	}
	r.events = append(r.events, proxyEvent{proxyID: proxyID, result: result})
}

func (r *CoalescingRecorder) Flush(ctx context.Context) {
	r.flush(ctx)
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
	r.cancel()
	close(r.done)
	r.mu.Unlock()

	select {
	case <-r.stopped:
	case <-ctx.Done():
	}
	drainCtx, cancel := closeDrainContext(ctx)
	defer cancel()
	drained, failed := r.flushDrain(drainCtx)
	if failed > 0 {
		slog.Warn("health.shutdown.drained", "events", drained, "failed", failed)
		return
	}
	slog.Info("health.shutdown.drained", "events", drained)
}

func closeDrainContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), 2*time.Second)
	}
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), 2*time.Second)
}

func (r *CoalescingRecorder) run() {
	defer close(r.stopped)
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.Flush(r.ctx)
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

func (r *CoalescingRecorder) flush(ctx context.Context) int {
	drained, _ := r.flushDrain(ctx)
	return drained
}

func (r *CoalescingRecorder) flushDrain(ctx context.Context) (int, int) {
	if r == nil || r.store == nil {
		return 0, 0
	}
	events := r.drain()
	if len(events) == 0 {
		return 0, 0
	}
	failed := r.writeEvents(ctx, events)
	return len(events) - failed, failed
}

func (r *CoalescingRecorder) writeEvents(ctx context.Context, events []proxyEvent) int {
	failed := 0
	for _, delta := range r.coalesce(events) {
		key := delta.key
		bucket := delta.bucket
		if bucket.successCount > 0 {
			latencyMS := 0
			if bucket.latencyCount > 0 {
				latencyMS = bucket.latencySumMS / bucket.latencyCount
			}
			if err := r.recordOutcome(ctx, key.proxyID, storage.ProxyHealthDelta{
				Success:    true,
				Reward:     r.policy.SuccessReward * bucket.successCount,
				LatencyMS:  latencyMS,
				ObservedAt: time.Now().UTC(),
			}); err != nil {
				failed += bucket.successCount
			}
		}
		if bucket.hasFailure {
			penalty := r.policy.FailurePenalty[bucket.failureKind]
			if penalty <= 0 {
				penalty = r.policy.FailurePenalty[FailureUnknown]
			}
			if err := r.recordOutcome(ctx, key.proxyID, storage.ProxyHealthDelta{
				Success:               false,
				FailureKind:           bucket.failureKind.String(),
				FailureHint:           bucket.failureHint,
				Penalty:               penalty,
				MaxConsecutiveFailure: r.policy.CircuitOpenThreshold,
				BaseCooldown:          r.policy.CircuitBaseCooldown,
				MaxCooldown:           r.policy.CircuitMaxCooldown,
				ObservedAt:            time.Now().UTC(),
			}); err != nil {
				failed++
			}
		}
	}
	return failed
}

func (r *CoalescingRecorder) recordOutcome(ctx context.Context, proxyID string, delta storage.ProxyHealthDelta) error {
	err := r.store.RecordProxyOutcome(ctx, proxyID, delta)
	if err == nil {
		return nil
	}
	if retryableRecorderError(err) {
		err = r.store.RecordProxyOutcome(ctx, proxyID, delta)
		if err == nil {
			return nil
		}
	}
	slog.Warn("health.recorder.write_failed", "error_kind", recorderErrorKind(err))
	return err
}

func recorderErrorKind(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "network"
	}
	return "backend"
}

func retryableRecorderError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

type orderedProxyDelta struct {
	key    deltaKey
	bucket proxyBucket
}

func (r *CoalescingRecorder) coalesce(events []proxyEvent) []orderedProxyDelta {
	deltas := make([]orderedProxyDelta, 0, len(events))
	open := make(map[deltaKey]int)
	for _, event := range events {
		key := deltaKey{proxyID: event.proxyID}
		if event.result.Success {
			idx, ok := open[key]
			if !ok {
				deltas = append(deltas, orderedProxyDelta{key: key})
				idx = len(deltas) - 1
				open[key] = idx
			}
			bucket := deltas[idx].bucket
			bucket.successCount++
			if event.result.LatencyMS > 0 {
				bucket.latencySumMS += event.result.LatencyMS
				bucket.latencyCount++
			}
			deltas[idx].bucket = bucket
		} else {
			delete(open, key)
			deltas = append(deltas, orderedProxyDelta{
				key: key,
				bucket: proxyBucket{
					hasFailure:  true,
					failureKind: event.result.Kind,
					failureHint: event.result.Hint,
				},
			})
		}
	}
	return deltas
}
