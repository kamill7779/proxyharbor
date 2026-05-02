package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type config struct {
	BaseURL         string
	AdminKey        string
	KeyPepper       string
	TenantID        string
	Docker          bool
	DockerSkipBuild bool
	ComposeFile     string
	Timeout         time.Duration
	InstanceURLs    []string
}

type runner struct {
	cfg       config
	client    *http.Client
	base      string
	instances []string
}

type tenantKeyResponse struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

type leaseResponse struct {
	LeaseID   string    `json:"lease_id"`
	ProxyID   string    `json:"proxy_id"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expires_at"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {
	cfg := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hacachecheck:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.BaseURL, "base-url", envDefault("PROXYHARBOR_BASE_URL", "http://127.0.0.1:18081"), "ProxyHarbor HA load-balancer URL")
	flag.StringVar(&cfg.AdminKey, "admin-key", "", "admin key; defaults to PROXYHARBOR_ADMIN_KEY")
	flag.StringVar(&cfg.KeyPepper, "key-pepper", "", "key pepper used when -docker starts compose; defaults to PROXYHARBOR_KEY_PEPPER")
	flag.StringVar(&cfg.TenantID, "tenant", "ha-cache", "tenant id")
	flag.BoolVar(&cfg.Docker, "docker", false, "start docker-compose HA test topology")
	flag.BoolVar(&cfg.DockerSkipBuild, "docker-skip-build", false, "reuse existing proxyharbor:ha-test image when starting docker HA topology")
	flag.StringVar(&cfg.ComposeFile, "compose-file", "docker-compose.ha-test.yaml", "compose file used with -docker")
	flag.DurationVar(&cfg.Timeout, "timeout", 3*time.Minute, "overall timeout")
	instances := envDefault("PROXYHARBOR_INSTANCE_URLS", "http://127.0.0.1:18083,http://127.0.0.1:18084,http://127.0.0.1:18085")
	for _, value := range strings.Split(instances, ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			cfg.InstanceURLs = append(cfg.InstanceURLs, strings.TrimRight(value, "/"))
		}
	}
	flag.Parse()
	if cfg.AdminKey == "" {
		cfg.AdminKey = os.Getenv("PROXYHARBOR_ADMIN_KEY")
	}
	if cfg.KeyPepper == "" {
		cfg.KeyPepper = os.Getenv("PROXYHARBOR_KEY_PEPPER")
	}
	return cfg
}

func run(ctx context.Context, cfg config, stdout io.Writer) error {
	if cfg.AdminKey == "" {
		secret, err := randomHex(32)
		if err != nil {
			return err
		}
		cfg.AdminKey = secret
	}
	if cfg.KeyPepper == "" {
		secret, err := randomHex(32)
		if err != nil {
			return err
		}
		cfg.KeyPepper = secret
	}
	if cfg.Docker {
		cleanupCompose(cfg, 90*time.Second, "down", "-v", "--remove-orphans")
		if err := startDocker(ctx, cfg); err != nil {
			cleanupCompose(cfg, 90*time.Second, "down", "-v", "--remove-orphans")
			return err
		}
		defer cleanupCompose(cfg, 90*time.Second, "down", "-v", "--remove-orphans")
	}
	r := runner{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, base: strings.TrimRight(cfg.BaseURL, "/"), instances: cfg.InstanceURLs}
	if err := r.waitReady(ctx); err != nil {
		return err
	}
	checks := []struct {
		name string
		fn   func(context.Context) error
	}{
		{name: "setup", fn: r.setup},
		{name: "new key visible cross-instance", fn: r.checkNewKeyVisible},
		{name: "revoked key rejected cross-instance", fn: r.checkRevokedKeyRejected},
		{name: "disabled tenant rejects old key", fn: r.checkDisabledTenantRejectsOldKey},
		{name: "disabled proxy excluded", fn: r.checkDisabledProxyExcluded},
		{name: "lease revoke invalidates validate", fn: r.checkLeaseRevokeInvalidatesValidate},
		{name: "policy disable/update observed", fn: r.checkPolicyDisableAndUpdate},
		{name: "redis interruption recovery", fn: r.checkRedisInterruptionRecovery},
		{name: "auth refresh failure metric", fn: r.checkAuthRefreshFailureMetric},
		{name: "metrics low cardinality", fn: r.checkMetricsLowCardinality},
	}
	for _, check := range checks {
		started := time.Now()
		if err := check.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", check.name, err)
		}
		fmt.Fprintf(stdout, "ok %-34s %s\n", check.name, time.Since(started).Round(time.Millisecond))
	}
	return nil
}

func (r runner) setup(ctx context.Context) error {
	_, _, _ = r.request(ctx, http.MethodPost, "/admin/tenants", r.cfg.AdminKey, "", map[string]any{"id": r.cfg.TenantID, "display_name": "HA Cache Correctness"})
	if err := r.upsertProxy(ctx, "ha-cache-proxy-a", true); err != nil {
		return err
	}
	if err := r.upsertPolicy(ctx, true, 120); err != nil {
		return err
	}
	return nil
}

func (r runner) checkNewKeyVisible(ctx context.Context) error {
	key, _, err := r.issueTenantKey(ctx, "visible")
	if err != nil {
		return err
	}
	return r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		_, err := r.createLeaseAt(ctx, base, key, unique("new-key"))
		return err
	})
}

func (r runner) checkRevokedKeyRejected(ctx context.Context) error {
	key, keyID, err := r.issueTenantKey(ctx, "revoke")
	if err != nil {
		return err
	}
	if _, err := r.createLease(ctx, key, unique("before-revoke")); err != nil {
		return err
	}
	status, body, err := r.request(ctx, http.MethodDelete, "/admin/tenants/"+url.PathEscape(r.cfg.TenantID)+"/keys/"+url.PathEscape(keyID), r.cfg.AdminKey, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("revoke key status %d: %s", status, bodySummary(body))
	}
	return r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		status, body, err := r.createLeaseRawAt(ctx, base, key, unique("after-revoke"))
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			return nil
		}
		return fmt.Errorf("create with revoked key status %d, want 401: %s", status, bodySummary(body))
	})
}

func (r runner) checkDisabledTenantRejectsOldKey(ctx context.Context) error {
	tenantID := r.cfg.TenantID + "-disabled-" + shortHex()
	status, body, err := r.request(ctx, http.MethodPost, "/admin/tenants", r.cfg.AdminKey, "", map[string]any{"id": tenantID, "display_name": "Disabled Tenant"})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusConflict {
		return fmt.Errorf("create disabled-test tenant status %d: %s", status, bodySummary(body))
	}
	key, _, err := r.issueTenantKeyFor(ctx, tenantID, "before-disable")
	if err != nil {
		return err
	}
	oldTenant := r.cfg.TenantID
	r.cfg.TenantID = tenantID
	if _, err := r.createLease(ctx, key, unique("before-disable")); err != nil {
		r.cfg.TenantID = oldTenant
		return err
	}
	r.cfg.TenantID = oldTenant
	status, body, err = r.request(ctx, http.MethodPatch, "/admin/tenants/"+url.PathEscape(tenantID), r.cfg.AdminKey, "", map[string]any{"status": "disabled"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("disable tenant status %d: %s", status, bodySummary(body))
	}
	return r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		oldTenant := r.cfg.TenantID
		r.cfg.TenantID = tenantID
		status, body, err := r.createLeaseRawAt(ctx, base, key, unique("after-disable"))
		r.cfg.TenantID = oldTenant
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			return nil
		}
		return fmt.Errorf("create with disabled tenant key status %d, want 401: %s", status, bodySummary(body))
	})
}

func (r runner) checkDisabledProxyExcluded(ctx context.Context) error {
	key, _, err := r.issueTenantKey(ctx, "proxy-disable")
	if err != nil {
		return err
	}
	for _, proxyID := range []string{"ha-cache-proxy-a", "ha-cache-proxy-redis-down"} {
		if err := r.upsertProxy(ctx, proxyID, false); err != nil {
			return err
		}
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.upsertProxy(cleanupCtx, "ha-cache-proxy-a", true)
	}()
	return r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		status, body, err := r.createLeaseRawAt(ctx, base, key, unique("proxy-disabled"))
		if err != nil {
			return err
		}
		if status != http.StatusConflict {
			return fmt.Errorf("create with disabled proxy status %d, want 409: %s", status, bodySummary(body))
		}
		var errBody errorResponse
		_ = json.Unmarshal(body, &errBody)
		if errBody.Error == "no_healthy_proxy" {
			return nil
		}
		return fmt.Errorf("create with disabled proxy status %d, want no_healthy_proxy: %s", status, bodySummary(body))
	})
}

func (r runner) checkLeaseRevokeInvalidatesValidate(ctx context.Context) error {
	key, _, err := r.issueTenantKey(ctx, "lease-revoke")
	if err != nil {
		return err
	}
	lease, err := r.createLease(ctx, key, unique("lease-revoke"))
	if err != nil {
		return err
	}
	if lease.Password == "" {
		return errors.New("lease response missing password")
	}
	appliedBefore := map[string]float64{}
	if err := r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		if err := r.validateLeaseAt(ctx, base, lease, http.StatusOK, ""); err != nil {
			return err
		}
		value, err := r.cacheInvalidationMetricAt(ctx, base, "lease", "invalidate", "applied", "none")
		if err != nil {
			return err
		}
		appliedBefore[base] = value
		return nil
	}); err != nil {
		return err
	}
	status, body, err := r.request(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(lease.LeaseID), key, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("revoke lease status %d: %s", status, bodySummary(body))
	}
	return r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		if err := r.validateLeaseAt(ctx, base, lease, 0, "lease_revoked"); err != nil {
			return err
		}
		value, err := r.cacheInvalidationMetricAt(ctx, base, "lease", "invalidate", "applied", "none")
		if err != nil {
			return err
		}
		if value <= appliedBefore[base] {
			return fmt.Errorf("lease invalidation metric did not increase on %s", base)
		}
		return nil
	})
}

func (r runner) validateLeaseAt(ctx context.Context, base string, lease leaseResponse, wantStatus int, wantError string) error {
	path := "/v1/gateway/validate?tenant_id=" + url.QueryEscape(r.cfg.TenantID) + "&lease_id=" + url.QueryEscape(lease.LeaseID) + "&target=example.com"
	status, body, err := r.validateRequestAt(ctx, base, path, lease.Password)
	if err != nil {
		return err
	}
	if wantStatus != 0 && status != wantStatus {
		return fmt.Errorf("validate status %d, want %d: %s", status, wantStatus, bodySummary(body))
	}
	if wantError != "" {
		var errBody errorResponse
		_ = json.Unmarshal(body, &errBody)
		if errBody.Error != wantError {
			return fmt.Errorf("validate error %q, want %q: %s", errBody.Error, wantError, bodySummary(body))
		}
	}
	return nil
}

func (r runner) validateRequestAt(ctx context.Context, base, path, password string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("ProxyHarbor-Key", r.cfg.AdminKey)
	req.Header.Set("ProxyHarbor-Password", password)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func (r runner) checkPolicyDisableAndUpdate(ctx context.Context) error {
	key, _, err := r.issueTenantKey(ctx, "policy")
	if err != nil {
		return err
	}
	if err := r.upsertProxy(ctx, "ha-cache-proxy-a", true); err != nil {
		return err
	}
	if err := r.upsertPolicy(ctx, false, 120); err != nil {
		return err
	}
	if err := r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		status, body, err := r.createLeaseRawAt(ctx, base, key, unique("policy-disabled"))
		if err != nil {
			return err
		}
		var errBody errorResponse
		_ = json.Unmarshal(body, &errBody)
		if errBody.Error == "policy_denied" {
			return nil
		}
		return fmt.Errorf("create with disabled policy status %d, want policy_denied: %s", status, bodySummary(body))
	}); err != nil {
		return err
	}
	if err := r.upsertPolicy(ctx, true, 30); err != nil {
		return err
	}
	return r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		lease, err := r.createLeaseAtWithTTL(ctx, base, key, unique("policy-enabled"), 90)
		if err != nil {
			return err
		}
		if lease.LeaseID == "" {
			return errors.New("empty lease id")
		}
		if remaining := time.Until(lease.ExpiresAt); remaining <= 15*time.Second || remaining > 35*time.Second {
			return fmt.Errorf("lease expires_at %s remaining=%s, want updated policy ttl around 30s", lease.ExpiresAt.Format(time.RFC3339), remaining)
		}
		return nil
	})
}

func (r runner) forEachInstance(ctx context.Context, timeout time.Duration, fn func(base string) error) error {
	instances := r.instances
	if len(instances) == 0 {
		instances = []string{r.base}
	}
	for _, base := range instances {
		base := base
		if err := retryUntil(ctx, timeout, func() error { return fn(base) }); err != nil {
			return fmt.Errorf("instance %s: %w", base, err)
		}
	}
	return nil
}

func (r runner) checkRedisInterruptionRecovery(ctx context.Context) error {
	if !r.cfg.Docker {
		return nil
	}
	revokedKey, revokedKeyID, err := r.issueTenantKey(ctx, "redis-down-revoke")
	if err != nil {
		return err
	}
	if _, err := r.createLease(ctx, revokedKey, unique("redis-down-before-revoke")); err != nil {
		return err
	}
	if err := r.upsertProxy(ctx, "ha-cache-proxy-redis-down", true); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.upsertProxy(cleanupCtx, "ha-cache-proxy-redis-down", true)
	}()
	stop, cleanupStop, err := newComposeCommand(ctx, r.cfg, "stop", "redis")
	if err != nil {
		return err
	}
	defer cleanupStop()
	stop.Stdout = os.Stdout
	stop.Stderr = os.Stderr
	stop.Env = scrubSecretEnv(os.Environ())
	if err := stop.Run(); err != nil {
		return fmt.Errorf("stop redis: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = startRedis(cleanupCtx, r.cfg)
	}()
	status, body, err := r.request(ctx, http.MethodDelete, "/admin/tenants/"+url.PathEscape(r.cfg.TenantID)+"/keys/"+url.PathEscape(revokedKeyID), r.cfg.AdminKey, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("redis-down revoke key status %d: %s", status, bodySummary(body))
	}
	// Catalog/lease invalidation is Redis pub/sub-only in HA mode; while Redis is
	// down we verify auth falls back to store truth, then emit a fresh catalog
	// event after Redis recovers to prove subscribers resynchronize.
	if err := r.forEachInstance(ctx, 8*time.Second, func(base string) error {
		status, body, err := r.createLeaseRawAt(ctx, base, revokedKey, unique("redis-down-after-revoke-before-restart"))
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			return nil
		}
		return fmt.Errorf("redis-down fallback create with revoked key status %d, want 401: %s", status, bodySummary(body))
	}); err != nil {
		return err
	}
	key, _, err := r.issueTenantKey(ctx, "redis-down")
	if err != nil {
		return err
	}
	if err := startRedis(ctx, r.cfg); err != nil {
		return err
	}
	if err := r.waitAllReadySubscribed(ctx, 35*time.Second); err != nil {
		return err
	}
	if err := publishBadInvalidation(ctx, r.cfg); err != nil {
		return err
	}
	if err := r.forEachInstance(ctx, 35*time.Second, func(base string) error {
		_, err := r.createLeaseAt(ctx, base, key, unique("redis-recovery"))
		return err
	}); err != nil {
		return err
	}
	if err := r.forEachInstance(ctx, 35*time.Second, func(base string) error {
		status, body, err := r.createLeaseRawAt(ctx, base, revokedKey, unique("redis-down-after-revoke"))
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			return nil
		}
		return fmt.Errorf("create with redis-down revoked key status %d, want 401: %s", status, bodySummary(body))
	}); err != nil {
		return err
	}
	if err := r.upsertProxy(ctx, "ha-cache-proxy-a", false); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.upsertProxy(cleanupCtx, "ha-cache-proxy-a", true)
	}()
	if err := r.upsertProxy(ctx, "ha-cache-proxy-redis-down", false); err != nil {
		return err
	}
	return r.forEachInstance(ctx, 35*time.Second, func(base string) error {
		status, body, err := r.createLeaseRawAt(ctx, base, key, unique("redis-recovered-after-proxy-disable"))
		if err != nil {
			return err
		}
		if status != http.StatusConflict {
			return fmt.Errorf("create after redis recovery disabled proxy status %d, want 409: %s", status, bodySummary(body))
		}
		var errBody errorResponse
		_ = json.Unmarshal(body, &errBody)
		if errBody.Error == "no_healthy_proxy" {
			return nil
		}
		return fmt.Errorf("create after redis recovery disabled proxy status %d, want no_healthy_proxy: %s", status, bodySummary(body))
	})
}

func (r runner) checkAuthRefreshFailureMetric(ctx context.Context) error {
	if !r.cfg.Docker {
		return nil
	}
	baselineMetrics, err := r.aggregateMetrics(ctx)
	if err != nil {
		return err
	}
	baseline, _ := metricValue(baselineMetrics, `proxyharbor_auth_refresh_fail_total`)
	stop, cleanupStop, err := newComposeCommand(ctx, r.cfg, "stop", "mysql")
	if err != nil {
		return err
	}
	defer cleanupStop()
	stop.Stdout = os.Stdout
	stop.Stderr = os.Stderr
	stop.Env = scrubSecretEnv(os.Environ())
	if err := stop.Run(); err != nil {
		return fmt.Errorf("stop mysql: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_ = startMysql(cleanupCtx, r.cfg)
	}()
	if err := retryUntil(ctx, 15*time.Second, func() error {
		metrics, err := r.aggregateMetrics(ctx)
		if err != nil {
			return err
		}
		value, ok := metricValue(metrics, `proxyharbor_auth_refresh_fail_total`)
		if !ok || value <= baseline {
			return fmt.Errorf("auth refresh failure metric value=%g baseline=%g ok=%v, want increase", value, baseline, ok)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := startMysql(ctx, r.cfg); err != nil {
		return err
	}
	return r.waitAllReadySubscribed(ctx, 35*time.Second)
}

func (r runner) checkMetricsLowCardinality(ctx context.Context) error {
	if err := r.waitAllReadySubscribed(ctx, 5*time.Second); err != nil {
		return err
	}
	text, err := r.aggregateMetrics(ctx)
	if err != nil {
		return err
	}
	for _, want := range []string{
		`proxyharbor_cache_invalidation_total{cache="auth",action="refresh",transport="redis",result="received",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="auth",action="refresh",transport="redis",result="applied",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="catalog",action="invalidate",transport="redis",result="published",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="catalog",action="invalidate",transport="redis",result="received",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="catalog",action="invalidate",transport="redis",result="applied",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="lease",action="invalidate",transport="redis",result="published",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="lease",action="invalidate",transport="redis",result="received",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="lease",action="invalidate",transport="redis",result="applied",error_kind="none"}`,
		`proxyharbor_auth_refresh_success_total`,
	} {
		if value, ok := metricValue(text, want); !ok || value <= 0 {
			return fmt.Errorf("metrics series %s value=%g ok=%v, want > 0", want, value, ok)
		}
	}
	for _, want := range []string{
		`proxyharbor_cache_invalidation_total{cache="auth",action="refresh",transport="redis",result="dropped",error_kind="decode"}`,
		`proxyharbor_auth_refresh_fail_total`,
	} {
		if value, ok := metricValue(text, want); !ok || value <= 0 {
			return fmt.Errorf("metrics series %s value=%g ok=%v, want > 0", want, value, ok)
		}
	}

	for _, forbidden := range []string{"tenant_id=", "key_id=", "proxy_id=", "lease_id=", "request_id="} {
		if strings.Contains(text, forbidden) {
			return fmt.Errorf("metrics contain high-cardinality label %s", forbidden)
		}
	}
	return nil
}

func (r runner) aggregateMetrics(ctx context.Context) (string, error) {
	bases := r.instances
	if len(bases) == 0 {
		bases = []string{r.base}
	}
	var out strings.Builder
	for _, base := range bases {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/metrics", r.cfg.AdminKey, "", nil)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("metrics status %d from %s: %s", status, base, bodySummary(body))
		}
		out.Write(body)
		out.WriteByte('\n')
	}
	return out.String(), nil
}

func metricValue(body, series string) (float64, bool) {
	var total float64
	found := false
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, series+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, false
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, false
		}
		total += value
		found = true
	}
	return total, found
}

func (r runner) upsertProxy(ctx context.Context, id string, healthy bool) error {
	payload := map[string]any{"id": id, "endpoint": "http://example.com:8080", "healthy": healthy, "weight": 1, "health_score": 100}
	status, body, err := r.request(ctx, http.MethodPost, "/v1/proxies", r.cfg.AdminKey, "", payload)
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		return nil
	}
	if status != http.StatusConflict {
		return fmt.Errorf("create proxy status %d: %s", status, bodySummary(body))
	}
	status, body, err = r.request(ctx, http.MethodPut, "/v1/proxies/"+url.PathEscape(id), r.cfg.AdminKey, "", payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("update proxy status %d: %s", status, bodySummary(body))
	}
	return nil
}

func (r runner) upsertPolicy(ctx context.Context, enabled bool, ttl int64) error {
	payload := map[string]any{"id": "default", "name": "Default", "enabled": enabled, "ttl_seconds": ttl}
	status, body, err := r.request(ctx, http.MethodPost, "/v1/policies", r.cfg.AdminKey, "", payload)
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		return nil
	}
	status, body, err = r.request(ctx, http.MethodPut, "/v1/policies/default", r.cfg.AdminKey, "", payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("upsert policy status %d: %s", status, bodySummary(body))
	}
	return nil
}

func (r runner) issueTenantKey(ctx context.Context, label string) (string, string, error) {
	return r.issueTenantKeyFor(ctx, r.cfg.TenantID, label)
}

func (r runner) issueTenantKeyFor(ctx context.Context, tenantID, label string) (string, string, error) {
	status, body, err := r.request(ctx, http.MethodPost, "/admin/tenants/"+url.PathEscape(tenantID)+"/keys", r.cfg.AdminKey, "", map[string]any{"label": label, "purpose": "ha_cache_correctness"})
	if err != nil {
		return "", "", err
	}
	if status != http.StatusCreated {
		return "", "", fmt.Errorf("issue tenant key status %d: %s", status, bodySummary(body))
	}
	var resp tenantKeyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", err
	}
	return resp.Key, resp.KeyID, nil
}

func (r runner) createLease(ctx context.Context, tenantKey, idempotency string) (leaseResponse, error) {
	return r.createLeaseAt(ctx, r.base, tenantKey, idempotency)
}

func (r runner) createLeaseAt(ctx context.Context, base, tenantKey, idempotency string) (leaseResponse, error) {
	return r.createLeaseAtWithTTL(ctx, base, tenantKey, idempotency, 20)
}

func (r runner) createLeaseAtWithTTL(ctx context.Context, base, tenantKey, idempotency string, ttlSeconds int64) (leaseResponse, error) {
	status, body, err := r.createLeaseRawAtWithTTL(ctx, base, tenantKey, idempotency, ttlSeconds)
	if err != nil {
		return leaseResponse{}, err
	}
	if status != http.StatusCreated {
		return leaseResponse{}, fmt.Errorf("create lease status %d: %s", status, bodySummary(body))
	}
	var lease leaseResponse
	if err := json.Unmarshal(body, &lease); err != nil {
		return leaseResponse{}, err
	}
	return lease, nil
}

func (r runner) createLeaseRaw(ctx context.Context, tenantKey, idempotency string) (int, []byte, error) {
	return r.createLeaseRawAt(ctx, r.base, tenantKey, idempotency)
}

func (r runner) createLeaseRawAt(ctx context.Context, base, tenantKey, idempotency string) (int, []byte, error) {
	return r.createLeaseRawAtWithTTL(ctx, base, tenantKey, idempotency, 20)
}

func (r runner) createLeaseRawAtWithTTL(ctx context.Context, base, tenantKey, idempotency string, ttlSeconds int64) (int, []byte, error) {
	return r.requestAt(ctx, base, http.MethodPost, "/v1/leases", tenantKey, idempotency, createLeasePayload(idempotency, ttlSeconds))
}

func (r runner) request(ctx context.Context, method, path, key, idempotency string, payload any) (int, []byte, error) {
	return r.requestAt(ctx, r.base, method, path, key, idempotency, payload)
}

func (r runner) requestAt(ctx context.Context, base, method, path, key, idempotency string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("ProxyHarbor-Key", key)
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func createLeasePayload(idempotency string, ttlSeconds int64) map[string]any {
	if ttlSeconds <= 0 {
		ttlSeconds = 20
	}
	return map[string]any{
		"subject":      map[string]any{"subject_type": "workload", "subject_id": idempotency},
		"resource_ref": map[string]any{"kind": "url", "id": "https://example.com"},
		"ttl_seconds":  ttlSeconds,
	}
}

func (r runner) cacheInvalidationMetricAt(ctx context.Context, base, cacheName, action, result, errorKind string) (float64, error) {
	status, body, err := r.requestAt(ctx, base, http.MethodGet, "/metrics", r.cfg.AdminKey, "", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("metrics status %d: %s", status, bodySummary(body))
	}
	return prometheusMetricValue(body, "proxyharbor_cache_invalidation_total", map[string]string{
		"cache":      cacheName,
		"action":     action,
		"transport":  "redis",
		"result":     result,
		"error_kind": errorKind,
	})
}

func prometheusMetricValue(body []byte, name string, labels map[string]string) (float64, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		metric, valueText, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		labelText := ""
		if raw, found := strings.CutPrefix(metric, name+"{"); found {
			labelText = strings.TrimSuffix(raw, "}")
		}
		if !metricLabelsMatch(labelText, labels) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(valueText), 64)
		if err != nil {
			return 0, err
		}
		return value, nil
	}
	return 0, fmt.Errorf("metric %s not found", name)
}

func metricLabelsMatch(raw string, want map[string]string) bool {
	if len(want) == 0 {
		return raw == ""
	}
	have := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return false
		}
		have[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	return true
}

func (r runner) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		status, body, err := r.request(ctx, http.MethodGet, "/readyz", "", "", nil)
		if err == nil && status == http.StatusOK {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("status=%d body=%s", status, bodySummary(body))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("readyz timeout: %s", last)
}

func retryUntil(ctx context.Context, timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if last == nil {
		last = errors.New("condition not met")
	}
	return last
}

func startDocker(ctx context.Context, cfg config) error {
	if !cfg.DockerSkipBuild {
		build := exec.CommandContext(ctx, "docker", "build", "--pull=false", "-t", "proxyharbor:ha-test", ".")
		build.Stdout = os.Stdout
		build.Stderr = os.Stderr
		build.Env = scrubSecretEnv(os.Environ())
		if err := build.Run(); err != nil {
			return fmt.Errorf("docker build: %w", err)
		}
	}
	args := []string{"up", "-d", "--wait", "--force-recreate"}
	if cfg.DockerSkipBuild {
		args = append(args, "--no-build")
	}
	cmd, cleanup, err := newComposeCommand(ctx, cfg, args...)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = scrubSecretEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

func (r runner) waitAllReadySubscribed(ctx context.Context, timeout time.Duration) error {
	return r.forEachInstance(ctx, timeout, func(base string) error {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/readyz", "", "", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("readyz status %d: %s", status, bodySummary(body))
		}
		if !strings.Contains(string(body), `"cache_invalidation"`) || !strings.Contains(string(body), `"state":"subscribed"`) {
			return fmt.Errorf("readyz missing subscribed invalidation state: %s", bodySummary(body))
		}
		return nil
	})
}

func publishBadInvalidation(ctx context.Context, cfg config) error {
	cmd, cleanup, err := newComposeCommand(ctx, cfg, "exec", "-T", "redis", "redis-cli", "PUBLISH", "proxyharbor:cache:invalidate", "{")
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	cmd.Env = scrubSecretEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("publish bad invalidation: %w", err)
	}
	return nil
}

func startRedis(ctx context.Context, cfg config) error {
	cmd, cleanup, err := newComposeCommand(ctx, cfg, "up", "-d", "--wait", "redis")
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = scrubSecretEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start redis: %w", err)
	}
	return nil
}

func startMysql(ctx context.Context, cfg config) error {
	cmd, cleanup, err := newComposeCommand(ctx, cfg, "up", "-d", "--wait", "mysql")
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = scrubSecretEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start mysql: %w", err)
	}
	return nil
}

func cleanupCompose(cfg config, timeout time.Duration, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd, cleanup, err := newComposeCommand(ctx, cfg, args...)
	if err != nil {
		return
	}
	defer cleanup()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = scrubSecretEnv(os.Environ())
	_ = cmd.Run()
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newComposeCommand(ctx context.Context, cfg config, args ...string) (*exec.Cmd, func(), error) {
	file, err := os.CreateTemp("", "proxyharbor-compose-*.env")
	if err != nil {
		return nil, func() {}, err
	}
	path := file.Name()
	content := "PROXYHARBOR_ADMIN_KEY=" + envFileValue(cfg.AdminKey) + "\n" +
		"PROXYHARBOR_KEY_PEPPER=" + envFileValue(cfg.KeyPepper) + "\n"
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, func() {}, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, func() {}, err
	}
	fullArgs := []string{"compose", "--env-file", path, "-f", cfg.ComposeFile}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	return cmd, func() { _ = os.Remove(path) }, nil
}

func envFileValue(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	return strings.ReplaceAll(value, "\n", "")
}

func scrubSecretEnv(env []string) []string {
	out := env[:0]
	for _, entry := range env {
		if isSecretEnv(entry) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isSecretEnv(entry string) bool {
	for _, prefix := range []string{
		"PROXYHARBOR_ADMIN_KEY=",
		"PROXYHARBOR_KEY_PEPPER=",
		"PROXYHARBOR_MYSQL_DSN=",
		"PROXYHARBOR_REDIS_PASSWORD=",
		"PROXYHARBOR_TENANT_KEY=",
	} {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func bodySummary(body []byte) string {
	var doc struct {
		Error  string `json:"error"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		switch {
		case doc.Error != "":
			return fmt.Sprintf("<redacted len=%d error=%q>", len(body), doc.Error)
		case doc.Status != "":
			return fmt.Sprintf("<redacted len=%d status=%q>", len(body), doc.Status)
		}
	}
	return fmt.Sprintf("<redacted len=%d>", len(body))
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func shortHex() string {
	v, err := randomHex(4)
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return v
}

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), shortHex())
}
