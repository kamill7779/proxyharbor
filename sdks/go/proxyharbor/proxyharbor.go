// Package proxyharbor is the official Go SDK for the ProxyHarbor service.
//
// Minimal usage:
//
//	proxy, err := proxyharbor.GetProxy(ctx)
//	if err != nil { return err }
//	fmt.Println(proxy.URL)
//
// Configure the SDK via environment variables:
//
//	PROXYHARBOR_BASE_URL=http://proxyharbor:8080
//	PROXYHARBOR_TENANT_KEY=tenant_key_xxx
//
// For admin-only inventory operations:
//
//	PROXYHARBOR_ADMIN_KEY=admin_key_xxx
package proxyharbor

import (
	"context"
	"sync"
)

// --- Package-level helpers ---

var (
	defaultClient   *Client
	defaultClientMu sync.Mutex
)

// Default returns the lazily-constructed package-level Client. The first
// successful call caches the client; subsequent calls reuse it.
func Default() (*Client, error) {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	if defaultClient != nil {
		return defaultClient, nil
	}
	c, err := New()
	if err != nil {
		return nil, err
	}
	defaultClient = c
	return defaultClient, nil
}

// SetDefault replaces the package-level Client. Useful for tests and for
// applications that want to centralise configuration.
func SetDefault(c *Client) {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	defaultClient = c
}

// GetProxy is a package-level shortcut for Default().GetProxy.
func GetProxy(ctx context.Context, opts ...GetProxyOption) (Proxy, error) {
	c, err := Default()
	if err != nil {
		return Proxy{}, err
	}
	return c.GetProxy(ctx, opts...)
}

// GetProxyURL is a package-level shortcut for Default().GetProxyURL.
func GetProxyURL(ctx context.Context, opts ...GetProxyOption) (string, error) {
	c, err := Default()
	if err != nil {
		return "", err
	}
	return c.GetProxyURL(ctx, opts...)
}

// Release is a package-level shortcut for Default().Release.
func Release(ctx context.Context, opts ...ReleaseOption) error {
	c, err := Default()
	if err != nil {
		return err
	}
	return c.Release(ctx, opts...)
}
