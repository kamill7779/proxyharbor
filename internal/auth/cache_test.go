package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDynamicStoreSnapshotClassifiesLastError(t *testing.T) {
	store := &memoryKeyStore{}
	d, err := NewDynamicStore(store, []byte("pepper-with-at-least-thirty-two-bytes"), time.Hour)
	if err != nil {
		t.Fatalf("NewDynamicStore() error = %v", err)
	}
	store.versionErr = errors.New("mysql password=super-secret-dsn failed")
	if err := d.RefreshNow(context.Background()); err == nil {
		t.Fatal("RefreshNow() error = nil, want store error")
	}
	snap := d.Snapshot()
	if snap.LastError == "" {
		t.Fatal("Snapshot().LastError = empty, want classified error")
	}
	if strings.Contains(snap.LastError, "super-secret-dsn") || strings.Contains(snap.LastError, "password=") {
		t.Fatalf("Snapshot().LastError leaked raw error: %q", snap.LastError)
	}
}
