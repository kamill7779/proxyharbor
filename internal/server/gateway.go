package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/version" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if r.Method == http.MethodConnect {
		s.gatewayConnect(w, r)
		return
	}
	s.gatewayHTTP(w, r)
}

func (s *Server) gatewayHTTP(w http.ResponseWriter, r *http.Request) {
	tenantID, leaseID, password, ok := gatewayCredentials(r)
	if !ok {
		respond(w, nil, domain.ErrAuthFailed, http.StatusOK)
		return
	}
	target := r.URL.Host
	if target == "" {
		target = r.Host
	}
	lease, proxy, err := s.svc.ValidateGatewayRequest(r.Context(), tenantID, leaseID, password, target)
	if err != nil {
		respond(w, nil, err, http.StatusOK)
		return
	}
	proxyURL, err := url.Parse(proxy.Endpoint)
	if err != nil {
		respond(w, nil, domain.ErrNoHealthyProxy, http.StatusOK)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = cloneURL(r.URL)
	out.Header = r.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("ProxyHarbor-Tenant")
	out.Header.Del("ProxyHarbor-Lease")
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	started := time.Now()
	resp, err := transport.RoundTrip(out)
	if err != nil {
		s.recordGatewayAudit(r.Context(), lease, "gateway_http_error", target, err.Error())
		respond(w, nil, domain.ErrNoHealthyProxy, http.StatusOK)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	written, _ := io.Copy(w, resp.Body)
	s.recordGatewayUsage(r.Context(), lease, 0, written)
	s.recordGatewayAudit(r.Context(), lease, "gateway_http_forward", target, time.Since(started).String())
}

func (s *Server) gatewayConnect(w http.ResponseWriter, r *http.Request) {
	tenantID, leaseID, password, ok := gatewayCredentials(r)
	if !ok {
		respond(w, nil, domain.ErrAuthFailed, http.StatusOK)
		return
	}
	lease, proxy, err := s.svc.ValidateGatewayRequest(r.Context(), tenantID, leaseID, password, r.Host)
	if err != nil {
		respond(w, nil, err, http.StatusOK)
		return
	}
	proxyURL, err := url.Parse(proxy.Endpoint)
	if err != nil {
		respond(w, nil, domain.ErrNoHealthyProxy, http.StatusOK)
		return
	}
	proxyConn, err := net.DialTimeout("tcp", proxyURL.Host, 10*time.Second)
	if err != nil {
		s.recordGatewayAudit(r.Context(), lease, "gateway_connect_error", r.Host, err.Error())
		respond(w, nil, domain.ErrNoHealthyProxy, http.StatusOK)
		return
	}
	defer proxyConn.Close()
	connectRequest := "CONNECT " + r.Host + " HTTP/1.1\r\nHost: " + r.Host + "\r\n"
	if proxyURL.User != nil {
		connectRequest += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(proxyURL.User.String())) + "\r\n"
	}
	_, _ = io.WriteString(proxyConn, connectRequest+"\r\n")
	proxyReader := bufio.NewReader(proxyConn)
	proxyResp, err := http.ReadResponse(proxyReader, r)
	if err != nil || proxyResp.StatusCode/100 != 2 {
		s.recordGatewayAudit(r.Context(), lease, "gateway_connect_error", r.Host, "upstream_connect_failed")
		respond(w, nil, domain.ErrNoHealthyProxy, http.StatusOK)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		respond(w, nil, domain.ErrUnsupported, http.StatusOK)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()
	_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	done := make(chan struct{}, 2)
	go proxyCopy(done, proxyConn, clientConn)
	go proxyCopy(done, clientConn, proxyReader)
	<-done
	s.recordGatewayUsage(r.Context(), lease, 0, 0)
	s.recordGatewayAudit(r.Context(), lease, "gateway_connect_forward", r.Host, "ok")
}

func gatewayCredentials(r *http.Request) (string, string, string, bool) {
	tenantID := r.Header.Get("ProxyHarbor-Tenant")
	if tenantID == "" {
		tenantID = "default"
	}
	leaseID := r.Header.Get("ProxyHarbor-Lease")
	password := ""
	if username, pass, ok := r.BasicAuth(); ok {
		leaseID = username
		password = pass
	}
	if username, pass, ok := basicProxyAuth(r.Header.Get("Proxy-Authorization")); ok {
		leaseID = username
		password = pass
	}
	if password == "" {
		password = r.Header.Get("ProxyHarbor-Password")
	}
	return tenantID, leaseID, password, leaseID != ""
}

func basicProxyAuth(header string) (string, string, bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
	if err != nil {
		return "", "", false
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	return username, password, ok
}

func cloneURL(in *url.URL) *url.URL {
	out := *in
	return &out
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func proxyCopy(done chan<- struct{}, dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
	done <- struct{}{}
}

func (s *Server) recordGatewayUsage(ctx context.Context, lease domain.Lease, sent, recv int64) {
	_ = s.svc.RecordUsage(ctx, []domain.UsageEvent{{EventID: "usage_" + time.Now().UTC().Format("20060102150405.000000000"), TenantID: lease.TenantID, LeaseID: lease.ID, BytesSent: sent, BytesRecv: recv, OccurredAt: time.Now().UTC()}})
}

func (s *Server) recordGatewayAudit(ctx context.Context, lease domain.Lease, action, resource, hint string) {
	_ = s.svc.RecordGatewayFeedback(ctx, []domain.AuditEvent{{EventID: "audit_" + time.Now().UTC().Format("20060102150405.000000000"), TenantID: lease.TenantID, PrincipalID: lease.Subject.StableID(), Action: action, Resource: resource, OccurredAt: time.Now().UTC(), Metadata: map[string]string{"hint": hint}}})
}
