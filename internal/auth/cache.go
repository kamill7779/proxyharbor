package auth

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

var staleCounter int64

func recordStale() { staleCounter++ }

// StaleCount returns the number of times the cache has been marked stale.
func StaleCount() int64 {
	return staleCounter
}

type entry struct {
	tenantID  string
	keyID     string
	expiresAt time.Time
	purpose   string
}

// DynamicStore holds an in-memory cache of tenant keys with background refresh.
type DynamicStore struct {
	mu      sync.RWMutex
	byHash  map[[32]byte]entry
	version int64
	store   KeyStore
	pepper  []byte
	refresh time.Duration
}

// NewDynamicStore creates a DynamicStore and performs an initial full load.
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
		return nil, err
	}
	d.applyRows(rows)
	v, err := d.store.GetTenantKeysVersion(ctx)
	if err != nil {
		return nil, err
	}
	d.version = v
	return d, nil
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
	v, err := d.store.GetTenantKeysVersion(ctx)
	if err != nil {
		recordStale()
		return
	}
	d.mu.RLock()
	cur := d.version
	d.mu.RUnlock()
	if v == cur {
		return
	}
	rows, err := d.store.GetTenantKeysSince(ctx, time.Now().Add(-2*d.refresh))
	if err != nil {
		recordStale()
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range rows {
		if r.RevokedAt != nil {
			delete(d.byHash, r.KeyHash)
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
}

func (d *DynamicStore) applyRows(rows []TenantKeyRow) {
	for _, r := range rows {
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
