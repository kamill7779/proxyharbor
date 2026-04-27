package server

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
)

func TestGatewayCredentialsParsesTenantScopedProxyUsername(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("tenant-a|lease-1:secret")))

	tenantID, leaseID, password, ok := gatewayCredentials(req)
	if !ok {
		t.Fatal("gatewayCredentials() ok = false")
	}
	if tenantID != "tenant-a" || leaseID != "lease-1" || password != "secret" {
		t.Fatalf("gatewayCredentials() = tenant %q lease %q password %q, want tenant-a lease-1 secret", tenantID, leaseID, password)
	}
}

func TestGatewayCredentialsKeepsLegacyProxyUsername(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("ProxyHarbor-Tenant", "tenant-a")
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("lease-1:secret")))

	tenantID, leaseID, password, ok := gatewayCredentials(req)
	if !ok {
		t.Fatal("gatewayCredentials() ok = false")
	}
	if tenantID != "tenant-a" || leaseID != "lease-1" || password != "secret" {
		t.Fatalf("gatewayCredentials() = tenant %q lease %q password %q, want tenant-a lease-1 secret", tenantID, leaseID, password)
	}
}
