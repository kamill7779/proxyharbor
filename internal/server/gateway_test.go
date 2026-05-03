package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/control/health"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
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

func TestGatewayConnectClosesTunnelOnShutdownContext(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	proxyClosed := make(chan struct{})
	go func() {
		defer close(proxyClosed)
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_, _ = io.Copy(io.Discard, reader)
	}()

	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://localhost:8080")
	svc.SetAllowInternalProxyEndpoint(true)
	if _, err := store.UpsertProxy(context.Background(), domain.Proxy{ID: "proxy-a", Endpoint: "http://" + proxyLn.Addr().String(), Healthy: true, Weight: 1, HealthScore: 100}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	lease, err := svc.CreateLease(context.Background(), domain.Principal{TenantID: "default"}, "idem-connect-shutdown", control.CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "subj"},
		ResourceRef: domain.ResourceRef{Kind: "host", ID: "example.com"},
		TTLSeconds:  60,
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}

	shutdownCtx, cancel := context.WithCancel(context.Background())
	handler := NewWithOptions(
		svc,
		auth.NewDynamicKeys(nil).WithAdminKey("admin-key-with-at-least-thirty-two-bytes"),
		Options{
			AdminStore:            NewMemoryAdminStore(),
			Pepper:                "pepper-with-at-least-thirty-two-bytes",
			ShutdownContext:       shutdownCtx,
			GatewayConnectTimeout: time.Second,
		},
	)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	clientConn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	credentials := base64.StdEncoding.EncodeToString([]byte(lease.Username + ":" + lease.Password))
	_, _ = fmt.Fprintf(clientConn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Basic %s\r\n\r\n", credentials)
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("ReadResponse() error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = handler.WaitForGatewayTunnels(waitCtx)
	waitCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForGatewayTunnels before shutdown error = %v, want deadline exceeded while tunnel is active", err)
	}

	cancel()
	select {
	case <-proxyClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy tunnel did not close after shutdown context cancellation")
	}
	waitCtx, waitCancel = context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := handler.WaitForGatewayTunnels(waitCtx); err != nil {
		t.Fatalf("WaitForGatewayTunnels after shutdown error = %v", err)
	}
}

func TestGatewayConnectCancelsPendingHandshakeOnShutdownContext(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		close(accepted)
		defer conn.Close()
		<-time.After(10 * time.Second)
	}()

	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://localhost:8080")
	svc.SetAllowInternalProxyEndpoint(true)
	if _, err := store.UpsertProxy(context.Background(), domain.Proxy{ID: "proxy-a", Endpoint: "http://" + proxyLn.Addr().String(), Healthy: true, Weight: 1, HealthScore: 100}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	lease, err := svc.CreateLease(context.Background(), domain.Principal{TenantID: "default"}, "idem-connect-handshake-shutdown", control.CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "subj"},
		ResourceRef: domain.ResourceRef{Kind: "host", ID: "example.com"},
		TTLSeconds:  60,
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}

	shutdownCtx, cancel := context.WithCancel(context.Background())
	handler := NewWithOptions(
		svc,
		auth.NewDynamicKeys(nil).WithAdminKey("admin-key-with-at-least-thirty-two-bytes"),
		Options{
			AdminStore:            NewMemoryAdminStore(),
			Pepper:                "pepper-with-at-least-thirty-two-bytes",
			ShutdownContext:       shutdownCtx,
			GatewayConnectTimeout: 5 * time.Second,
		},
	)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	clientConn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	credentials := base64.StdEncoding.EncodeToString([]byte(lease.Username + ":" + lease.Password))
	_, _ = fmt.Fprintf(clientConn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Basic %s\r\n\r\n", credentials)

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("proxy did not accept CONNECT handshake")
	}

	cancel()
	readDone := make(chan error, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(clientConn), &http.Request{Method: http.MethodConnect})
		if resp != nil {
			_ = resp.Body.Close()
		}
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err == nil {
			return
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pending CONNECT handshake did not unblock after shutdown context cancellation")
	}
}

func TestGatewayFailureLogUsesBoundedFields(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	handler := &Server{}
	handler.logGatewayFailure("connect", domain.Lease{
		TenantID: "tenant-secret",
		ID:       "lease-secret",
		ProxyID:  "proxy-secret",
	}, "sensitive.example:443", health.FailureTimeout, "timeout")

	body := logs.String()
	for _, forbidden := range []string{"tenant_id", "lease_id", "proxy_id", "tenant-secret", "lease-secret", "proxy-secret", "sensitive.example"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("gateway failure log leaked %q in %s", forbidden, body)
		}
	}
	for _, want := range []string{"mode=connect", "kind=timeout", "reason=timeout", "target_present=true"} {
		if !strings.Contains(body, want) {
			t.Fatalf("gateway failure log missing %q in %s", want, body)
		}
	}
}

func TestWaitForTunnelCopiesWaitsForSecondCompletion(t *testing.T) {
	done := make(chan struct{}, 2)
	closed := make(chan struct{}, 1)
	finished := make(chan struct{})

	go func() {
		waitForTunnelCopies(done, func() {
			close(closed)
		})
		close(finished)
	}()

	done <- struct{}{}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("waitForTunnelCopies did not close connections after first copy completion")
	}
	select {
	case <-finished:
		t.Fatal("waitForTunnelCopies returned before second copy completion")
	case <-time.After(50 * time.Millisecond):
	}
	done <- struct{}{}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("waitForTunnelCopies did not wait for second copy completion")
	}
}
