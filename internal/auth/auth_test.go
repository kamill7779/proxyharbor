package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func reqWithKey(key string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(HeaderName, key)
	return r
}

func reqWithKeyAndTenant(key, tenant string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(HeaderName, key)
	r.Header.Set(TenantHeaderName, tenant)
	return r
}

// fakeKeyStore implements KeyStore for tests.
type fakeKeyStore struct {
	keys    []TenantKeyRow
	version int64
}

func (f *fakeKeyStore) GetTenantKeys(ctx context.Context) ([]TenantKeyRow, error) { return f.keys, nil }
func (f *fakeKeyStore) GetTenantKeysSince(ctx context.Context, since time.Time) ([]TenantKeyRow, error) {
	return f.keys, nil
}
func (f *fakeKeyStore) GetTenantKeysVersion(ctx context.Context) (int64, error) {
	return f.version, nil
}
func (f *fakeKeyStore) IncrementTenantKeysVersion(ctx context.Context) error        { return nil }
func (f *fakeKeyStore) CreateTenantKey(ctx context.Context, key TenantKeyRow) error { return nil }
func (f *fakeKeyStore) RevokeTenantKey(ctx context.Context, keyID string) error     { return nil }
func (f *fakeKeyStore) GetTenant(ctx context.Context, tenantID string) (TenantRow, error) {
	return TenantRow{}, nil
}

// U5: Admin Key + Tenant Key coexist, admin hit优先, type=admin.
func TestAuthenticator_AdminPriority(t *testing.T) {
	pepper := []byte("pepper-1234567890123456789012345678")
	adminKey := "admin-super-secret-key-32bytes"
	tenantKey := "tenant-key-12345678901234567890"
	hash := sha256Pepper(pepper, tenantKey)

	fake := &fakeKeyStore{
		keys: []TenantKeyRow{
			{ID: "k1", TenantID: "t1", KeyHash: hash},
		},
		version: 1,
	}
	ds, _ := NewDynamicStore(fake, pepper, time.Hour)
	a := NewDynamicKeys(ds).WithAdminKey(adminKey)

	p, err := a.Authenticate(reqWithKey(adminKey))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Type != "admin" {
		t.Fatalf("expected admin, got %s", p.Type)
	}

	p, err = a.Authenticate(reqWithKey(tenantKey))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Type != "tenant_key" || p.TenantID != "t1" {
		t.Fatalf("unexpected principal: %+v", p)
	}
}

// U6: Tenant Key + wrong X-On-Behalf-Of -> 403 / ErrTenantMismatch.
func TestAuthenticator_TenantMismatch(t *testing.T) {
	pepper := []byte("pepper-1234567890123456789012345678")
	tenantKey := "tenant-key-12345678901234567890"
	hash := sha256Pepper(pepper, tenantKey)

	fake := &fakeKeyStore{
		keys: []TenantKeyRow{
			{ID: "k1", TenantID: "t1", KeyHash: hash},
		},
		version: 1,
	}
	ds, _ := NewDynamicStore(fake, pepper, time.Hour)
	a := NewDynamicKeys(ds)

	_, err := a.Authenticate(reqWithKeyAndTenant(tenantKey, "t2"))
	if err != domain.ErrTenantMismatch {
		t.Fatalf("expected ErrTenantMismatch, got %v", err)
	}
}

// U7: same key + different pepper -> different hash (no collision).
func TestHash_PepperCollision(t *testing.T) {
	key := "same-key"
	h1 := sha256Pepper([]byte("pepper-one-32-bytes-long!!!!!"), key)
	h2 := sha256Pepper([]byte("pepper-two-32-bytes-long!!!!!"), key)
	if h1 == h2 {
		t.Fatal("expected different hashes for different peppers")
	}
}

// Three mode switching test.
func TestAuthenticator_ModeSwitching(t *testing.T) {
	a := NewLegacy("legacy-key")
	p, err := a.Authenticate(reqWithKey("legacy-key"))
	if err != nil || p.Type != "header_key" {
		t.Fatalf("legacy mode failed: %+v %v", p, err)
	}

	a = NewTenantKeys(map[string]string{"tk": "tnt"})
	p, err = a.Authenticate(reqWithKey("tk"))
	if err != nil || p.Type != "tenant_key" || p.TenantID != "tnt" {
		t.Fatalf("tenant-keys mode failed: %+v %v", p, err)
	}

	pepper := []byte("pepper-1234567890123456789012345678")
	k := "dynamic-k"
	h := sha256Pepper(pepper, k)
	fake := &fakeKeyStore{keys: []TenantKeyRow{{ID: "k1", TenantID: "t1", KeyHash: h}}, version: 1}
	ds, _ := NewDynamicStore(fake, pepper, time.Hour)
	a = NewDynamicKeys(ds)
	p, err = a.Authenticate(reqWithKey(k))
	if err != nil || p.Type != "tenant_key" || p.TenantID != "t1" {
		t.Fatalf("dynamic-keys mode failed: %+v %v", p, err)
	}
}

func TestValidateTenantKeysRejectsMalformedMappings(t *testing.T) {
	cases := []string{
		"broken",
		"tenant:",
		":key",
		"Tenant:key",
		",",
	}
	for _, raw := range cases {
		if err := ValidateTenantKeys(raw); err == nil {
			t.Fatalf("ValidateTenantKeys(%q) expected error", raw)
		}
	}
}

func TestValidateTenantKeysAcceptsValidMappings(t *testing.T) {
	if err := ValidateTenantKeys("tenant_a:key_a, tenant-b:key-b"); err != nil {
		t.Fatalf("ValidateTenantKeys returned error: %v", err)
	}
}
