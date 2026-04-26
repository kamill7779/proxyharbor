package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/control/health"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

const Version = "0.1.0-alpha"

type Role string

const (
	RoleAll        Role = "all"
	RoleController Role = "controller"
	RoleGateway    Role = "gateway"
)

type Server struct {
	mux            *http.ServeMux
	svc            *control.Service
	authn          *auth.Authenticator
	role           Role
	healthRecorder health.HealthRecorder
	dependency     storage.DependencyChecker
}

func New(svc *control.Service, authn *auth.Authenticator) http.Handler {
	return NewForRole(svc, authn, RoleAll)
}

func NewForRole(svc *control.Service, authn *auth.Authenticator, role Role) http.Handler {
	s := &Server{mux: http.NewServeMux(), svc: svc, authn: authn, role: role}
	s.routes()
	return Recover(s)
}

func NewForRoleWithHealthRecorder(svc *control.Service, authn *auth.Authenticator, role Role, recorder health.HealthRecorder) http.Handler {
	return NewForRoleWithHealthRecorderAndDependencies(svc, authn, role, recorder, nil)
}

func NewForRoleWithHealthRecorderAndDependencies(svc *control.Service, authn *auth.Authenticator, role Role, recorder health.HealthRecorder, dependency storage.DependencyChecker) http.Handler {
	s := &Server{mux: http.NewServeMux(), svc: svc, authn: authn, role: role, healthRecorder: recorder, dependency: dependency}
	s.routes()
	return Recover(s)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect && (s.role == RoleAll || s.role == RoleGateway) {
		s.gateway(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/readyz", s.ready)
	s.mux.HandleFunc("/version", s.version)
	if s.role == RoleAll || s.role == RoleController {
		s.mux.HandleFunc("/v1/leases", s.leases)
		s.mux.HandleFunc("/v1/leases/", s.leaseByID)
		s.mux.HandleFunc("/v1/providers", s.providers)
		s.mux.HandleFunc("/v1/providers/", s.providerByID)
		s.mux.HandleFunc("/v1/proxies", s.proxies)
		s.mux.HandleFunc("/v1/proxies/", s.proxyByID)
		s.mux.HandleFunc("/v1/policies", s.policies)
		s.mux.HandleFunc("/v1/policies/", s.policyByID)
		s.mux.HandleFunc("/v1/catalog/latest", s.catalog)
		s.mux.HandleFunc("/v1/internal/usage-events:batch", s.usage)
		s.mux.HandleFunc("/v1/internal/gateway-feedback:batch", s.gatewayFeedback)
	}
	if s.role == RoleAll || s.role == RoleController || s.role == RoleGateway {
		s.mux.HandleFunc("/v1/gateway/validate", s.validate)
	}
	if s.role == RoleAll || s.role == RoleGateway {
		s.mux.HandleFunc("/", s.gateway)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "role": string(s.role)})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	if s.dependency == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "role": string(s.role), "reasons": map[string]string{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	checks := s.dependency.CheckDependencies(ctx)
	reasons := make(map[string]string, len(checks))
	ready := true
	for name, err := range checks {
		if err != nil {
			reasons[name] = "unavailable"
			ready = false
			continue
		}
		reasons[name] = "ok"
	}
	status := http.StatusOK
	bodyStatus := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		bodyStatus = "degraded"
	}
	writeJSON(w, status, map[string]any{"status": bodyStatus, "role": string(s.role), "reasons": reasons})
}
func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": Version, "stability": "alpha", "role": string(s.role)})
}

func (s *Server) leases(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	var req control.CreateLeaseRequest
	if !decode(w, r, &req) {
		return
	}
	lease, err := s.svc.CreateLease(r.Context(), principal, r.Header.Get("Idempotency-Key"), req)
	respond(w, lease, err, http.StatusCreated)
}

func (s *Server) leaseByID(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
	if strings.HasSuffix(path, ":renew") {
		if !allow(w, r, http.MethodPost) {
			return
		}
		out, err := s.svc.RenewLease(r.Context(), principal, strings.TrimSuffix(path, ":renew"))
		respond(w, out, err, http.StatusOK)
		return
	}
	if !allow(w, r, http.MethodDelete) {
		return
	}
	respond(w, map[string]string{"status": "revoked"}, s.svc.RevokeLease(r.Context(), principal, path), http.StatusOK)
}

func (s *Server) policies(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		policies, err := s.svc.ListPolicies(r.Context(), principal.TenantID)
		respond(w, map[string]any{"policies": policies}, err, http.StatusOK)
	case http.MethodPost:
		var policy domain.Policy
		if !decode(w, r, &policy) {
			return
		}
		out, err := s.svc.CreatePolicy(r.Context(), principal, policy)
		respond(w, out, err, http.StatusCreated)
	default:
		method(w)
	}
}

func (s *Server) policyByID(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/policies/")
	switch r.Method {
	case http.MethodGet:
		out, err := s.svc.GetPolicy(r.Context(), principal, id)
		respond(w, out, err, http.StatusOK)
	case http.MethodPut:
		var policy domain.Policy
		if !decode(w, r, &policy) {
			return
		}
		out, err := s.svc.UpdatePolicy(r.Context(), principal, id, policy)
		respond(w, out, err, http.StatusOK)
	case http.MethodDelete:
		respond(w, map[string]string{"status": "deleted"}, s.svc.DeletePolicy(r.Context(), principal, id), http.StatusOK)
	default:
		method(w)
	}
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		providers, err := s.svc.ListProviders(r.Context(), principal.TenantID)
		respond(w, map[string]any{"providers": providers}, err, http.StatusOK)
	case http.MethodPost:
		var provider domain.Provider
		if !decode(w, r, &provider) {
			return
		}
		out, err := s.svc.CreateProvider(r.Context(), principal, provider)
		respond(w, out, err, http.StatusCreated)
	default:
		method(w)
	}
}

func (s *Server) providerByID(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/providers/")
	switch r.Method {
	case http.MethodGet:
		out, err := s.svc.GetProvider(r.Context(), principal, id)
		respond(w, out, err, http.StatusOK)
	case http.MethodPut:
		var provider domain.Provider
		if !decode(w, r, &provider) {
			return
		}
		out, err := s.svc.UpdateProvider(r.Context(), principal, id, provider)
		respond(w, out, err, http.StatusOK)
	case http.MethodDelete:
		respond(w, map[string]string{"status": "deleted"}, s.svc.DeleteProvider(r.Context(), principal, id), http.StatusOK)
	default:
		method(w)
	}
}

func (s *Server) proxies(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		proxies, err := s.svc.ListProxies(r.Context(), principal.TenantID)
		respond(w, map[string]any{"proxies": proxies}, err, http.StatusOK)
	case http.MethodPost:
		var proxy domain.Proxy
		if !decode(w, r, &proxy) {
			return
		}
		out, err := s.svc.CreateProxy(r.Context(), principal, proxy)
		respond(w, out, err, http.StatusCreated)
	default:
		method(w)
	}
}

func (s *Server) proxyByID(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/proxies/")
	if strings.HasSuffix(path, ":health") {
		if !allow(w, r, http.MethodPost) {
			return
		}
		var body struct {
			Healthy     bool   `json:"healthy"`
			FailureHint string `json:"failure_hint,omitempty"`
		}
		if !decode(w, r, &body) {
			return
		}
		out, err := s.svc.UpdateProxyHealth(r.Context(), principal, strings.TrimSuffix(path, ":health"), body.Healthy, body.FailureHint)
		respond(w, out, err, http.StatusOK)
		return
	}
	switch r.Method {
	case http.MethodGet:
		out, err := s.svc.GetProxy(r.Context(), principal, path)
		respond(w, out, err, http.StatusOK)
	case http.MethodPut:
		var proxy domain.Proxy
		if !decode(w, r, &proxy) {
			return
		}
		out, err := s.svc.UpdateProxy(r.Context(), principal, path, proxy)
		respond(w, out, err, http.StatusOK)
	case http.MethodDelete:
		respond(w, map[string]string{"status": "deleted"}, s.svc.DeleteProxy(r.Context(), principal, path), http.StatusOK)
	default:
		method(w)
	}
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	out, err := s.svc.Catalog(r.Context(), principal.TenantID)
	respond(w, out, err, http.StatusOK)
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		Events []domain.UsageEvent `json:"events"`
	}
	if !decode(w, r, &body) {
		return
	}
	for i := range body.Events {
		body.Events[i].TenantID = principal.TenantID
	}
	respond(w, map[string]int{"accepted": len(body.Events)}, s.svc.RecordUsage(r.Context(), body.Events), http.StatusAccepted)
}
func (s *Server) gatewayFeedback(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		Events []domain.AuditEvent `json:"events"`
	}
	if !decode(w, r, &body) {
		return
	}
	for i := range body.Events {
		body.Events[i].TenantID = principal.TenantID
		if body.Events[i].Action == "" {
			body.Events[i].Action = "gateway_feedback"
		}
		if body.Events[i].OccurredAt.IsZero() {
			body.Events[i].OccurredAt = time.Now().UTC()
		}
	}
	respond(w, map[string]int{"accepted": len(body.Events)}, s.svc.RecordGatewayFeedback(r.Context(), body.Events), http.StatusAccepted)
}
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	tenant := r.URL.Query().Get("tenant_id")
	if tenant == "" {
		tenant = "default"
	}
	lease, err := s.svc.ValidateLease(r.Context(), tenant, r.URL.Query().Get("lease_id"), r.URL.Query().Get("password"), r.URL.Query().Get("target"))
	respond(w, lease, err, http.StatusOK)
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	p, err := s.authn.Authenticate(r)
	if err != nil {
		respond(w, nil, err, http.StatusOK)
		return domain.Principal{}, false
	}
	return p, true
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		respond(w, nil, err, http.StatusOK)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, body any, err error, okStatus int) {
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, domain.ErrAuthFailed) {
			status = http.StatusUnauthorized
		} else if errors.Is(err, domain.ErrTenantMismatch) {
			status = http.StatusForbidden
		} else if errors.Is(err, domain.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": domain.ErrorCode(err)})
		return
	}
	writeJSON(w, okStatus, body)
}
func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		methodNotAllowed(w)
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func method(w http.ResponseWriter) { methodNotAllowed(w) }
func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
}

var _ storage.Store
