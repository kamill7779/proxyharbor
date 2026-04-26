package health

import (
	"context"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/storage"
)

type recordingStore struct {
	deltas []storage.ProxyHealthDelta
}

func (s *recordingStore) RecordProxyOutcome(_ context.Context, tenantID, proxyID string, delta storage.ProxyHealthDelta) error {
	s.deltas = append(s.deltas, delta)
	return nil
}

func TestRecorderCoalescesByTenantAndProxy(t *testing.T) {
	store := &recordingStore{}
	recorder := NewCoalescingRecorder(store, RecorderOptions{BufferSize: 8, FlushInterval: time.Hour})

	recorder.RecordProxyResult(context.Background(), "tenant-a", "proxy-1", ProxyHealthResult{Success: true, LatencyMS: 120, Hint: "ok"})
	recorder.RecordProxyResult(context.Background(), "tenant-a", "proxy-1", ProxyHealthResult{Success: false, Kind: FailureTimeout, Hint: "timeout"})
	recorder.Flush(context.Background())

	if len(store.deltas) != 2 {
		t.Fatalf("deltas = %d, want 2", len(store.deltas))
	}
	if !store.deltas[0].Success || store.deltas[0].Reward != 5 || store.deltas[0].LatencyMS != 120 {
		t.Fatalf("success delta = %+v", store.deltas[0])
	}
	if store.deltas[1].Success || store.deltas[1].FailureKind != "timeout" || store.deltas[1].Penalty != 15 || store.deltas[1].FailureHint != "timeout" {
		t.Fatalf("failure delta = %+v", store.deltas[1])
	}
}
