package health

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/storage"
)

type blockingOutcomeStore struct {
	called chan struct{}
}

func (s blockingOutcomeStore) RecordProxyOutcome(ctx context.Context, _ string, _ storage.ProxyHealthDelta) error {
	select {
	case <-s.called:
	default:
		close(s.called)
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestCloseCancelsInFlightTickerFlush(t *testing.T) {
	store := blockingOutcomeStore{called: make(chan struct{})}
	recorder := NewCoalescingRecorder(store, RecorderOptions{BufferSize: 1, FlushInterval: time.Hour})
	recorder.RecordProxyResult(context.Background(), "proxy-a", ProxyHealthResult{Success: true, LatencyMS: 10})
	go recorder.Flush(recorder.ctx)

	select {
	case <-store.called:
	case <-time.After(time.Second):
		t.Fatal("ticker flush did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		recorder.Close(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Close did not cancel in-flight ticker flush")
	}
}

type contextCheckingOutcomeStore struct {
	calls       atomic.Int32
	sawCanceled atomic.Bool
}

func (s *contextCheckingOutcomeStore) RecordProxyOutcome(ctx context.Context, _ string, _ storage.ProxyHealthDelta) error {
	s.calls.Add(1)
	if ctx.Err() != nil {
		s.sawCanceled.Store(true)
		return ctx.Err()
	}
	return nil
}

func TestCloseUsesFreshDrainContextWhenCallerContextCanceled(t *testing.T) {
	store := &contextCheckingOutcomeStore{}
	recorder := NewCoalescingRecorder(store, RecorderOptions{BufferSize: 1, FlushInterval: time.Hour})
	recorder.RecordProxyResult(context.Background(), "proxy-a", ProxyHealthResult{Success: true, LatencyMS: 10})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder.Close(ctx)

	if store.calls.Load() == 0 {
		t.Fatal("Close did not drain buffered health event")
	}
	if store.sawCanceled.Load() {
		t.Fatal("Close drained buffered health event with canceled caller context")
	}
}

func TestCloseDoesNotExtendExpiredDeadlineContext(t *testing.T) {
	store := &contextCheckingOutcomeStore{}
	recorder := NewCoalescingRecorder(store, RecorderOptions{BufferSize: 1, FlushInterval: time.Hour})
	recorder.RecordProxyResult(context.Background(), "proxy-a", ProxyHealthResult{Success: true, LatencyMS: 10})

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	recorder.Close(ctx)

	if store.calls.Load() == 0 {
		t.Fatal("Close did not attempt to drain buffered health event")
	}
	if !store.sawCanceled.Load() {
		t.Fatal("Close extended an expired deadline context")
	}
}

type failingOutcomeStore struct{}

func (failingOutcomeStore) RecordProxyOutcome(context.Context, string, storage.ProxyHealthDelta) error {
	return errors.New("backend failed for proxy-secret")
}

func TestRecorderWriteFailureLogUsesBoundedFields(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	recorder := NewCoalescingRecorder(failingOutcomeStore{}, RecorderOptions{BufferSize: 1, FlushInterval: time.Hour})
	recorder.RecordProxyResult(context.Background(), "proxy-secret", ProxyHealthResult{Success: true, LatencyMS: 10})
	recorder.Flush(context.Background())

	body := logs.String()
	for _, forbidden := range []string{"proxy_id", "proxy-secret", "backend failed for proxy-secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("health recorder log leaked %q in %s", forbidden, body)
		}
	}
	if !strings.Contains(body, "error_kind=backend") {
		t.Fatalf("health recorder log missing bounded error kind in %s", body)
	}
}
