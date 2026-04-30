package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/control/health"
	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

const Version = "0.4.6"

type gatewayValidationResponse struct {
	ID             string             `json:"lease_id"`
	TenantID       string             `json:"tenant_id"`
	Generation     int64              `json:"lease_generation"`
	Subject        domain.Subject     `json:"subject"`
	ResourceRef    domain.ResourceRef `json:"resource_ref"`
	PolicyRef      domain.PolicyRef   `json:"policy_ref"`
	GatewayURL     string             `json:"gateway_url"`
	Username       string             `json:"username"`
	ProxyID        string             `json:"proxy_id"`
	ExpiresAt      time.Time          `json:"expires_at"`
	RenewBefore    time.Time          `json:"renew_before"`
	CatalogVersion string             `json:"catalog_version"`
	CandidateSetID string             `json:"candidate_set_id"`
}

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
	adminStore     AdminStore
	pepper         string
	role           Role
	healthRecorder health.HealthRecorder
	dependency     storage.DependencyChecker
	authReady      AuthReadyChecker
	invalidator    auth.Invalidator
	instanceID     string
	authSnapshot   AuthSnapshotProvider
	clusterStore   storage.ClusterStore
	clusterSummary map[string]any
}

// AuthReadyChecker reports whether the auth subsystem is ready to handle
// traffic. Implementations must return a non-nil error when traffic should
// not be accepted yet (e.g. dynamic auth cache not yet loaded).
type AuthReadyChecker interface {
	CheckAuthReady(context.Context) error
}

// AuthSnapshotProvider returns a non-secret view of the auth cache state.
type AuthSnapshotProvider interface {
	AuthSnapshot() auth.Snapshot
}

// Options bundles optional dependencies for the HTTP server.
type Options struct {
	Role           Role
	HealthRecorder health.HealthRecorder
	Dependency     storage.DependencyChecker
	AdminStore     AdminStore
	Pepper         string
	AuthReady      AuthReadyChecker
	AuthSnapshot   AuthSnapshotProvider
	Invalidator    auth.Invalidator
	InstanceID     string
	ClusterStore   storage.ClusterStore
	ClusterSummary map[string]any
}

// NewWithOptions builds the HTTP handler with the supplied optional
// dependencies. Fields left zero are treated as not configured.
func NewWithOptions(svc *control.Service, authn *auth.Authenticator, opts Options) http.Handler {
	role := opts.Role
	if role == "" {
		role = RoleAll
	}
	s := &Server{
		mux:            http.NewServeMux(),
		svc:            svc,
		authn:          authn,
		role:           role,
		healthRecorder: opts.HealthRecorder,
		dependency:     opts.Dependency,
		adminStore:     opts.AdminStore,
		pepper:         opts.Pepper,
		authReady:      opts.AuthReady,
		authSnapshot:   opts.AuthSnapshot,
		invalidator:    opts.Invalidator,
		instanceID:     opts.InstanceID,
		clusterStore:   opts.ClusterStore,
		clusterSummary: opts.ClusterSummary,
	}
	s.routes()
	return Recover(s)
}

func (s *Server) debugAuthCache(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	body := map[string]any{
		"instance_id": s.instanceID,
		"role":        string(s.role),
	}
	if s.authSnapshot != nil {
		body["cache"] = s.authSnapshot.AuthSnapshot()
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) debugAuthCacheMetrics(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	body := map[string]any{
		"instance_id": s.instanceID,
		"role":        string(s.role),
	}
	if s.authSnapshot != nil {
		snap := s.authSnapshot.AuthSnapshot()
		body["auth_cache_initialized"] = snap.Initialized
		body["auth_cache_version"] = snap.Version
		body["auth_cache_entries"] = snap.Entries
		body["auth_cache_last_refresh"] = snap.LastRefresh
		body["auth_cache_stale_seconds"] = snap.StaleSeconds
		body["auth_cache_refresh_failures"] = snap.RefreshFailures
		body["auth_cache_refresh_success"] = snap.RefreshSuccess
		if snap.LastError != "" {
			body["auth_cache_last_error"] = snap.LastError
		}
	}
	writeJSON(w, http.StatusOK, body)
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
	s.mux.HandleFunc("/metrics", s.requireAdminAuth(metrics.Handler().ServeHTTP))
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
	if s.adminStore != nil && (s.role == RoleAll || s.role == RoleController) {
		admin := newAdminHandler(s.adminStore, s.pepper, s.invalidator, s.instanceID)
		admin.register(s.mux, s.requireAdminAuth)
		s.mux.HandleFunc("/admin/cluster", s.requireAdminAuth(s.adminCluster))
	}
	if s.role == RoleAll || s.role == RoleController {
		s.mux.HandleFunc("/debug/auth-cache", s.requireAdminAuth(s.debugAuthCache))
		s.mux.HandleFunc("/debug/auth-cache/metrics", s.requireAdminAuth(s.debugAuthCacheMetrics))
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	body := map[string]any{"status": "ok", "role": string(s.role)}
	writeJSON(w, http.StatusOK, body)
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	reasons := map[string]string{}
	ready := true
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if s.dependency != nil {
		for name, err := range s.dependency.CheckDependencies(ctx) {
			if err != nil {
				reasons[name] = "unavailable"
				ready = false
				continue
			}
			reasons[name] = "ok"
		}
	}
	if s.authReady != nil {
		if err := s.authReady.CheckAuthReady(ctx); err != nil {
			reasons["auth_cache"] = "not_initialized"
			ready = false
		} else {
			reasons["auth_cache"] = "ok"
		}
	}
	body := map[string]any{"role": string(s.role), "reasons": reasons}
	if s.instanceID != "" {
		body["instance_id"] = s.instanceID
	}
	status := http.StatusOK
	body["status"] = "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		body["status"] = "degraded"
	}
	writeJSON(w, status, body)
}
func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": Version, "stability": "release-candidate", "role": string(s.role)})
}

func (s *Server) leases(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	principal, ok = s.requireTenant(w, r, principal)
	if !ok {
		return
	}
	var req control.CreateLeaseRequest
	if !decode(w, r, &req) {
		return
	}
	lease, err := s.svc.CreateLease(r.Context(), principal, r.Header.Get("Idempotency-Key"), req)
	metrics.LeaseCreateTotal.Inc()
	if err != nil {
		metrics.LeaseCreateFail.Inc()
	}
	respond(w, lease, err, http.StatusCreated)
}

func (s *Server) leaseByID(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	principal, ok = s.requireTenant(w, r, principal)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
	if strings.HasSuffix(path, ":renew") {
		if !allow(w, r, http.MethodPost) {
			return
		}
		out, err := s.svc.RenewLease(r.Context(), principal, strings.TrimSuffix(path, ":renew"))
		metrics.LeaseRenewTotal.Inc()
		if err != nil {
			metrics.LeaseRenewFail.Inc()
		}
		respond(w, out, err, http.StatusOK)
		return
	}
	if !allow(w, r, http.MethodDelete) {
		return
	}
	err := s.svc.RevokeLease(r.Context(), principal, path)
	metrics.LeaseRevokeTotal.Inc()
	if err != nil {
		metrics.LeaseRevokeFail.Inc()
	}
	respond(w, map[string]string{"status": "revoked"}, err, http.StatusOK)
}

func (s *Server) policies(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !s.requireAdmin(w, principal) {
			return
		}
		policies, err := s.svc.ListPolicies(r.Context())
		respond(w, map[string]any{"policies": policies}, err, http.StatusOK)
	case http.MethodPost:
		if !s.requireAdmin(w, principal) {
			return
		}
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
		if !s.requireAdmin(w, principal) {
			return
		}
		out, err := s.svc.GetPolicy(r.Context(), principal, id)
		respond(w, out, err, http.StatusOK)
	case http.MethodPut:
		if !s.requireAdmin(w, principal) {
			return
		}
		var policy domain.Policy
		if !decode(w, r, &policy) {
			return
		}
		out, err := s.svc.UpdatePolicy(r.Context(), principal, id, policy)
		respond(w, out, err, http.StatusOK)
	case http.MethodDelete:
		if !s.requireAdmin(w, principal) {
			return
		}
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
		if !s.requireAdmin(w, principal) {
			return
		}
		providers, err := s.svc.ListProviders(r.Context())
		respond(w, map[string]any{"providers": providers}, err, http.StatusOK)
	case http.MethodPost:
		if !s.requireAdmin(w, principal) {
			return
		}
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
		if !s.requireAdmin(w, principal) {
			return
		}
		out, err := s.svc.GetProvider(r.Context(), principal, id)
		respond(w, out, err, http.StatusOK)
	case http.MethodPut:
		if !s.requireAdmin(w, principal) {
			return
		}
		var provider domain.Provider
		if !decode(w, r, &provider) {
			return
		}
		out, err := s.svc.UpdateProvider(r.Context(), principal, id, provider)
		respond(w, out, err, http.StatusOK)
	case http.MethodDelete:
		if !s.requireAdmin(w, principal) {
			return
		}
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
		if !s.requireAdmin(w, principal) {
			return
		}
		proxies, err := s.svc.ListProxies(r.Context())
		respond(w, map[string]any{"proxies": proxies}, err, http.StatusOK)
	case http.MethodPost:
		if !s.requireAdmin(w, principal) {
			return
		}
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
		if !s.requireAdmin(w, principal) {
			return
		}
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
		if !s.requireAdmin(w, principal) {
			return
		}
		out, err := s.svc.GetProxy(r.Context(), principal, path)
		respond(w, out, err, http.StatusOK)
	case http.MethodPut:
		if !s.requireAdmin(w, principal) {
			return
		}
		var proxy domain.Proxy
		if !decode(w, r, &proxy) {
			return
		}
		out, err := s.svc.UpdateProxy(r.Context(), principal, path, proxy)
		respond(w, out, err, http.StatusOK)
	case http.MethodDelete:
		if !s.requireAdmin(w, principal) {
			return
		}
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
	if !s.requireAdmin(w, principal) {
		return
	}
	out, err := s.svc.Catalog(r.Context())
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
	if !s.requireAdmin(w, principal) {
		return
	}
	var body struct {
		Events []domain.UsageEvent `json:"events"`
	}
	if !decode(w, r, &body) {
		return
	}
	for i := range body.Events {
		if body.Events[i].TenantID == "" {
			respond(w, nil, domain.ErrBadRequest, http.StatusOK)
			return
		}
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
	if !s.requireAdmin(w, principal) {
		return
	}
	var body struct {
		Events []domain.AuditEvent `json:"events"`
	}
	if !decode(w, r, &body) {
		return
	}
	for i := range body.Events {
		if body.Events[i].TenantID == "" {
			respond(w, nil, domain.ErrBadRequest, http.StatusOK)
			return
		}
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
	principal, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	if !s.requireAdmin(w, principal) {
		return
	}
	tenant := r.URL.Query().Get("tenant_id")
	if tenant == "" {
		tenant = "default"
	}
	lease, err := s.svc.ValidateLease(r.Context(), tenant, r.URL.Query().Get("lease_id"), r.URL.Query().Get("password"), r.URL.Query().Get("target"))
	metrics.LeaseValidateTotal.Inc()
	if err != nil {
		metrics.LeaseValidateFail.Inc()
	}
	if err != nil {
		respond(w, nil, err, http.StatusOK)
		return
	}
	respond(w, gatewayValidationFromLease(lease), nil, http.StatusOK)
}

func gatewayValidationFromLease(lease domain.Lease) gatewayValidationResponse {
	return gatewayValidationResponse{
		ID:             lease.ID,
		TenantID:       lease.TenantID,
		Generation:     lease.Generation,
		Subject:        lease.Subject,
		ResourceRef:    lease.ResourceRef,
		PolicyRef:      lease.PolicyRef,
		GatewayURL:     lease.GatewayURL,
		Username:       lease.Username,
		ProxyID:        lease.ProxyID,
		ExpiresAt:      lease.ExpiresAt,
		RenewBefore:    lease.RenewBefore,
		CatalogVersion: lease.CatalogVersion,
		CandidateSetID: lease.CandidateSetID,
	}
}

// requireAdminAuth wraps a handler to enforce admin-only access for /admin/* routes.
func (s *Server) requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := s.requireAuth(w, r)
		if !ok {
			return
		}
		if principal.Type != "admin" {
			respond(w, nil, domain.ErrForbidden, http.StatusOK)
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAdmin(w http.ResponseWriter, principal domain.Principal) bool {
	if principal.Type == "admin" {
		return true
	}
	respond(w, nil, domain.ErrForbidden, http.StatusOK)
	return false
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	p, err := s.authn.Authenticate(r)
	if err != nil {
		slog.Warn("auth.failure", "path", r.URL.Path, "method", r.Method, "err", err)
		respond(w, nil, err, http.StatusOK)
		return domain.Principal{}, false
	}
	return p, true
}

// requireTenant enforces On-Behalf-Of semantics per the design doc.
func (s *Server) requireTenant(w http.ResponseWriter, r *http.Request, principal domain.Principal) (domain.Principal, bool) {
	// Admin calling /admin/* does not need On-Behalf-Of.
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		if principal.Type == "admin" {
			return principal, true
		}
		respond(w, nil, domain.ErrForbidden, http.StatusOK)
		return domain.Principal{}, false
	}

	obo := r.Header.Get(auth.OnBehalfOfHeader)

	if principal.Type == "admin" {
		if obo == "" {
			respond(w, nil, domain.NewKindedError(domain.ErrBadRequest, domain.ErrorKindUnknown, "missing_on_behalf_of", nil), http.StatusOK)
			return domain.Principal{}, false
		}
		// Verify tenant exists and is active.
		if s.adminStore != nil {
			tenant, err := s.adminStore.GetTenant(r.Context(), obo)
			if err != nil {
				respond(w, nil, domain.ErrTenantNotFound, http.StatusOK)
				return domain.Principal{}, false
			}
			if !tenant.Enabled {
				respond(w, nil, domain.ErrTenantDisabled, http.StatusOK)
				return domain.Principal{}, false
			}
		}
		principal.TenantID = obo
		return principal, true
	}

	if principal.Type == "tenant_key" {
		if obo != "" && obo != principal.TenantID {
			respond(w, nil, domain.ErrTenantMismatch, http.StatusOK)
			return domain.Principal{}, false
		}
		return principal, true
	}

	respond(w, nil, domain.ErrForbidden, http.StatusOK)
	return domain.Principal{}, false
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		respond(w, nil, domain.NewKindedError(domain.ErrBadRequest, domain.ErrorKindUnknown, "invalid_json", nil), http.StatusOK)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, body any, err error, okStatus int) {
	if err != nil {
		if errors.Is(err, domain.ErrNoHealthyProxy) {
			metrics.NoHealthyProxy.Inc()
		}
		code := domain.ErrInternal
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, domain.ErrAuthFailed):
			status = http.StatusUnauthorized
			code = domain.ErrAuthFailed
		case errors.Is(err, domain.ErrTenantMismatch), errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrTenantDisabled):
			status = http.StatusForbidden
			code = publicError(err)
		case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrTenantNotFound):
			status = http.StatusNotFound
			code = publicError(err)
		case errors.Is(err, domain.ErrBadRequest), errors.Is(err, domain.ErrUnsafeDestination), errors.Is(err, domain.ErrSubjectNotEligible):
			status = http.StatusBadRequest
			code = publicError(err)
		case errors.Is(err, domain.ErrUnsupported):
			status = http.StatusNotImplemented
			code = domain.ErrUnsupported
		case errors.Is(err, domain.ErrPolicyDenied), errors.Is(err, domain.ErrNoHealthyProxy), errors.Is(err, domain.ErrIdempotencyConflict):
			status = http.StatusConflict
			code = publicError(err)
		case errors.Is(err, domain.ErrLeaseRevoked), errors.Is(err, domain.ErrStaleLease):
			status = http.StatusConflict
			code = publicError(err)
		case errors.Is(err, domain.ErrLeaseExpired):
			status = http.StatusGone
			code = domain.ErrLeaseExpired
		case errors.Is(err, domain.ErrCatalogStale):
			status = http.StatusConflict
			code = domain.ErrCatalogStale
		default:
			slog.Warn("server.response_unknown_error", "err", err)
		}
		payload := map[string]any{"error": domain.ErrorCode(code)}
		if code != domain.ErrInternal {
			if reason := domain.ErrorReason(err); reason != "" {
				payload["reason"] = reason
			}
		}
		writeJSON(w, status, payload)
		return
	}
	writeJSON(w, okStatus, body)
}

func publicError(err error) error {
	switch {
	case errors.Is(err, domain.ErrTenantMismatch):
		return domain.ErrTenantMismatch
	case errors.Is(err, domain.ErrForbidden):
		return domain.ErrForbidden
	case errors.Is(err, domain.ErrTenantDisabled):
		return domain.ErrTenantDisabled
	case errors.Is(err, domain.ErrNotFound):
		return domain.ErrNotFound
	case errors.Is(err, domain.ErrTenantNotFound):
		return domain.ErrTenantNotFound
	case errors.Is(err, domain.ErrBadRequest):
		return domain.ErrBadRequest
	case errors.Is(err, domain.ErrUnsafeDestination):
		return domain.ErrUnsafeDestination
	case errors.Is(err, domain.ErrSubjectNotEligible):
		return domain.ErrSubjectNotEligible
	case errors.Is(err, domain.ErrUnsupported):
		return domain.ErrUnsupported
	case errors.Is(err, domain.ErrPolicyDenied):
		return domain.ErrPolicyDenied
	case errors.Is(err, domain.ErrNoHealthyProxy):
		return domain.ErrNoHealthyProxy
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return domain.ErrIdempotencyConflict
	case errors.Is(err, domain.ErrLeaseRevoked):
		return domain.ErrLeaseRevoked
	case errors.Is(err, domain.ErrStaleLease):
		return domain.ErrStaleLease
	}
	return domain.ErrInternal
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
