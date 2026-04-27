package auth

import (
	"context"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"time"
)

var staleCounter atomic.Int64

func recordStale() { staleCounter.Add(1) }

// StaleCount returns the number of times the cache has been marked stale.
func StaleCount() int64 {
	return staleCounter.Load()
}

type entry struct {
	tenantID  string
	keyID     string
	expiresAt time.Time
	purpose   string
}

// Snapshot is a non-secret view of dynamic auth cache state safe to expose
// in /readyz, debug or metrics endpoints. It must never contain plaintext
// keys, key hashes, pepper, admin keys, or any tenant secret material.
type Snapshot struct {
	Initialized     bool      `json:"initialized"`
	Version         int64     `json:"version"`
	Entries         int       `json:"entries"`
	LastRefresh     time.Time `json:"last_refresh"`
	StaleSeconds    int64     `json:"stale_seconds"`
	LastError       string    `json:"last_error,omitempty"`
	RefreshFailures int64     `json:"refresh_failures"`
	RefreshSuccess  int64     `json:"refresh_success"`
}

// DynamicStore holds an in-memory cache of tenant keys with background refresh.
type DynamicStore struct {
	mu      sync.RWMutex
	byHash  map[[32]byte]entry
	version int64
	store   KeyStore
	pepper  []byte
	refresh time.Duration

	initialized     atomic.Bool
	lastRefreshUnix atomic.Int64
	lastError       atomic.Pointer[string]
	failures        atomic.Int64
	successes       atomic.Int64

	// refreshMu serializes RefreshNow/tick to avoid duplicate full loads.
	refreshMu sync.Mutex
}

// NewDynamicStore creates a DynamicStore and performs an initial full load.
// On success the store is marked Initialized. If the initial load fails the
// returned error must be treated as fatal by the caller; do not start the
// process with an uninitialized DynamicStore.
func NewDynamicStore(store KeyStore, pepper []byte, refresh time.Duration) (*DynamicStore, error) {
	d := &DynamicStore{
		byHash:  make(map[[32]byte]entry),
		store:   store,
		pepper:  pepper,
		refresh: refresh,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := d.store.GetTenantKeys(ctx)
	if err != nil {
		d.recordError(err)
		return nil, err
	}
	d.applyRows(rows)
	v, err := d.store.GetTenantKeysVersion(ctx)
	if err != nil {
		d.recordError(err)
		return nil, err
	}
	d.version = v
	d.markRefreshSuccess()
	d.initialized.Store(true)
	return d, nil
}

// Initialized reports whether the initial cache load has succeeded.
func (d *DynamicStore) Initialized() bool {
	if d == nil {
		return false
	}
	return d.initialized.Load()
}

// Snapshot returns a non-secret view of the current cache state.
func (d *DynamicStore) Snapshot() Snapshot {
	if d == nil {
		return Snapshot{}
	}
	d.mu.RLock()
	v := d.version
	entries := len(d.byHash)
	d.mu.RUnlock()
	lastUnix := d.lastRefreshUnix.Load()
	var last time.Time
	var stale int64
	if lastUnix > 0 {
		last = time.Unix(lastUnix, 0).UTC()
		stale = int64(time.Since(last).Seconds())
	}
	var lastErr string
	if p := d.lastError.Load(); p != nil {
		lastErr = *p
	}
	return Snapshot{
		Initialized:     d.initialized.Load(),
		Version:         v,
		Entries:         entries,
		LastRefresh:     last,
		StaleSeconds:    stale,
		LastError:       lastErr,
		RefreshFailures: d.failures.Load(),
		RefreshSuccess:  d.successes.Load(),
	}
}

// RefreshNow forces an immediate version check and full reload if the
// store version differs from the cached version. Safe to call concurrently
// with the background refresh loop.
func (d *DynamicStore) RefreshNow(ctx context.Context) error {
	if d == nil {
		return nil
	}
	return d.refreshLocked(ctx)
}

// Lookup checks the presented key against the local cache.
func (d *DynamicStore) Lookup(presented string) (entry, bool) {
	h := sha256Pepper(d.pepper, presented)
	d.mu.RLock()
	defer d.mu.RUnlock()
	e, ok := d.byHash[h]
	if !ok {
		return entry{}, false
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		return entry{}, false
	}
	return e, true
}

func (d *DynamicStore) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.byHash)
}

// Run starts the background refresh loop. It blocks until ctx is done.
func (d *DynamicStore) Run(ctx context.Context) {
	ticker := time.NewTicker(d.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.tick(ctx)
		}
	}
}

func (d *DynamicStore) tick(ctx context.Context) {
	_ = d.refreshLocked(ctx)
}

func (d *DynamicStore) refreshLocked(ctx context.Context) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	v, err := d.store.GetTenantKeysVersion(ctx)
	if err != nil {
		recordStale()
		d.recordError(err)
		return err
	}
	d.mu.RLock()
	cur := d.version
	d.mu.RUnlock()
	if v == cur {
		d.markRefreshSuccess()
		return nil
	}
	rows, err := d.store.GetTenantKeys(ctx)
	if err != nil {
		recordStale()
		d.recordError(err)
		return err
	}
	d.mu.Lock()
	d.byHash = make(map[[32]byte]entry, len(rows))
	for _, r := range rows {
		if r.RevokedAt != nil {
			continue
		}
		d.byHash[r.KeyHash] = entry{
			tenantID:  r.TenantID,
			keyID:     r.ID,
			expiresAt: zeroIfNil(r.ExpiresAt),
			purpose:   r.Purpose,
		}
	}
	d.version = v
	d.mu.Unlock()
	d.markRefreshSuccess()
	return nil
}

func (d *DynamicStore) markRefreshSuccess() {
	d.lastRefreshUnix.Store(time.Now().Unix())
	d.lastError.Store(nil)
	d.successes.Add(1)
}

func (d *DynamicStore) recordError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	d.lastError.Store(&msg)
	d.failures.Add(1)
}

func (d *DynamicStore) applyRows(rows []TenantKeyRow) {
	for _, r := range rows {
		if r.RevokedAt != nil {
			continue
		}
		d.byHash[r.KeyHash] = entry{
			tenantID:  r.TenantID,
			keyID:     r.ID,
			expiresAt: zeroIfNil(r.ExpiresAt),
			purpose:   r.Purpose,
		}
	}
}

func sha256Pepper(pepper []byte, key string) [32]byte {
	h := sha256.New()
	h.Write(pepper)
	h.Write([]byte(key))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func zeroIfNil(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
