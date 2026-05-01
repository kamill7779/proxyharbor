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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	BaseURL     string
	AdminKey    string
	KeyPepper   string
	TenantID    string
	Concurrency int
	Docker      bool
	ComposeFile string
	Timeout     time.Duration
}

type runner struct {
	cfg    config
	client *http.Client
	base   string
}

type tenantKeyResponse struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

type leaseResponse struct {
	LeaseID   string `json:"lease_id"`
	ProxyID   string `json:"proxy_id"`
	Password  string `json:"password"`
	ExpiresAt string `json:"expires_at"`
}

type errorResponse struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

func main() {
	cfg := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hacorrect:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.BaseURL, "base-url", envDefault("PROXYHARBOR_BASE_URL", "http://localhost:18081"), "ProxyHarbor HA load-balancer URL")
	flag.StringVar(&cfg.AdminKey, "admin-key", envDefault("PROXYHARBOR_ADMIN_KEY", ""), "admin key")
	flag.StringVar(&cfg.KeyPepper, "key-pepper", envDefault("PROXYHARBOR_KEY_PEPPER", ""), "key pepper used when -docker starts compose")
	flag.StringVar(&cfg.TenantID, "tenant", "ha-correct", "tenant id")
	flag.IntVar(&cfg.Concurrency, "concurrency", 24, "concurrent requests per scenario")
	flag.BoolVar(&cfg.Docker, "docker", false, "start docker-compose HA test topology")
	flag.StringVar(&cfg.ComposeFile, "compose-file", "docker-compose.ha-test.yaml", "compose file used with -docker")
	flag.DurationVar(&cfg.Timeout, "timeout", 4*time.Minute, "overall timeout")
	flag.Parse()
	if cfg.Concurrency < 2 {
		cfg.Concurrency = 2
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
		if err := startDocker(ctx, cfg); err != nil {
			return err
		}
	}
	r := runner{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}, base: strings.TrimRight(cfg.BaseURL, "/")}
	if err := r.waitReady(ctx); err != nil {
		return err
	}
	checks := []struct {
		name string
		fn   func(context.Context) error
	}{
		{name: "setup", fn: r.setup},
		{name: "idempotent concurrent create", fn: r.checkConcurrentIdempotentCreate},
		{name: "unique concurrent create", fn: r.checkConcurrentUniqueCreate},
		{name: "renew revoke race", fn: r.checkRenewRevokeRace},
		{name: "disabled tenant rejects key", fn: r.checkDisabledTenantRejectsKey},
	}
	for _, check := range checks {
		started := time.Now()
		if err := check.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", check.name, err)
		}
		fmt.Fprintf(stdout, "ok %-32s %s\n", check.name, time.Since(started).Round(time.Millisecond))
	}
	return nil
}

func (r runner) setup(ctx context.Context) error {
	_, _, _ = r.request(ctx, http.MethodPost, "/admin/tenants", r.cfg.AdminKey, "", map[string]any{"id": r.cfg.TenantID, "display_name": "HA Correctness"})
	for i := 0; i < 6; i++ {
		payload := map[string]any{"id": fmt.Sprintf("ha-proxy-%02d", i), "endpoint": fmt.Sprintf("http://127.0.0.1:%d", 19100+i), "healthy": true, "weight": 1 + i, "health_score": 100}
		status, body, err := r.request(ctx, http.MethodPost, "/v1/proxies", r.cfg.AdminKey, "", payload)
		if err != nil {
			return err
		}
		if status != http.StatusCreated && status != http.StatusConflict {
			return fmt.Errorf("seed proxy status %d: %s", status, body)
		}
	}
	return nil
}

func (r runner) checkConcurrentIdempotentCreate(ctx context.Context) error {
	tenantKey, err := r.issueTenantKey(ctx, "idem")
	if err != nil {
		return err
	}
	idem := fmt.Sprintf("same-%d", time.Now().UnixNano())
	leases, errs := r.concurrentCreate(ctx, tenantKey, idem, false)
	if err := firstErr(errs); err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, lease := range leases {
		ids[lease.LeaseID] = true
	}
	if len(ids) != 1 {
		return fmt.Errorf("idempotent create produced %d leases: %v", len(ids), sortedKeys(ids))
	}
	return nil
}

func (r runner) checkConcurrentUniqueCreate(ctx context.Context) error {
	tenantKey, err := r.issueTenantKey(ctx, "unique")
	if err != nil {
		return err
	}
	leases, errs := r.concurrentCreate(ctx, tenantKey, "unique", true)
	if err := firstErr(errs); err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, lease := range leases {
		ids[lease.LeaseID] = true
	}
	if len(ids) != len(leases) {
		return fmt.Errorf("unique create produced duplicate lease ids: %d unique for %d creates", len(ids), len(leases))
	}
	return nil
}

func (r runner) checkRenewRevokeRace(ctx context.Context) error {
	tenantKey, err := r.issueTenantKey(ctx, "race")
	if err != nil {
		return err
	}
	lease, err := r.createLease(ctx, tenantKey, fmt.Sprintf("race-%d", time.Now().UnixNano()), true)
	if err != nil {
		return err
	}
	var renewOK, conflictOrGone atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, r.cfg.Concurrency+1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		status, body, err := r.request(ctx, http.MethodDelete, "/v1/leases/"+url.PathEscape(lease.LeaseID), tenantKey, "", nil)
		if err != nil {
			errs <- err
			return
		}
		if status != http.StatusOK {
			errs <- fmt.Errorf("revoke status %d: %s", status, body)
		}
	}()
	for i := 0; i < r.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body, err := r.request(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(lease.LeaseID)+":renew", tenantKey, "", nil)
			if err != nil {
				errs <- err
				return
			}
			switch status {
			case http.StatusOK:
				renewOK.Add(1)
			case http.StatusConflict, http.StatusGone, http.StatusNotFound:
				conflictOrGone.Add(1)
			default:
				errs <- fmt.Errorf("renew status %d: %s", status, body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	if err := firstErrChan(errs); err != nil {
		return err
	}
	status, body, err := r.request(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(lease.LeaseID)+":renew", tenantKey, "", nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return fmt.Errorf("renew after revoke succeeded; body=%s renewOKDuringRace=%d conflicts=%d", body, renewOK.Load(), conflictOrGone.Load())
	}
	return nil
}

func (r runner) checkDisabledTenantRejectsKey(ctx context.Context) error {
	tenantID := fmt.Sprintf("disabled-%d", time.Now().UnixNano())
	status, body, err := r.request(ctx, http.MethodPost, "/admin/tenants", r.cfg.AdminKey, "", map[string]any{"id": tenantID, "display_name": "Disabled"})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("create disabled-test tenant status %d: %s", status, body)
	}
	status, body, err = r.request(ctx, http.MethodPatch, "/admin/tenants/"+url.PathEscape(tenantID), r.cfg.AdminKey, "", map[string]any{"status": "disabled"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("disable tenant status %d: %s", status, body)
	}
	status, body, err = r.request(ctx, http.MethodPost, "/admin/tenants/"+url.PathEscape(tenantID)+"/keys", r.cfg.AdminKey, "", map[string]any{"label": "after-disable"})
	if err != nil {
		return err
	}
	if status != http.StatusForbidden {
		return fmt.Errorf("create key for disabled tenant status %d, want 403: %s", status, body)
	}
	var errBody errorResponse
	_ = json.Unmarshal(body, &errBody)
	if errBody.Error != "tenant_disabled" {
		return fmt.Errorf("disabled tenant error = %q, want tenant_disabled; body=%s", errBody.Error, body)
	}
	return nil
}

func (r runner) concurrentCreate(ctx context.Context, tenantKey, idemPrefix string, unique bool) ([]leaseResponse, []error) {
	leases := make([]leaseResponse, r.cfg.Concurrency)
	errs := make([]error, r.cfg.Concurrency)
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idem := idemPrefix
			if unique {
				idem = fmt.Sprintf("%s-%d-%d", idemPrefix, time.Now().UnixNano(), i)
			}
			lease, err := r.createLease(ctx, tenantKey, idem, unique)
			leases[i] = lease
			errs[i] = err
		}(i)
	}
	wg.Wait()
	return leases, errs
}

func (r runner) issueTenantKey(ctx context.Context, label string) (string, error) {
	status, body, err := r.request(ctx, http.MethodPost, "/admin/tenants/"+url.PathEscape(r.cfg.TenantID)+"/keys", r.cfg.AdminKey, "", map[string]any{"label": label, "purpose": "ha_correctness"})
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("issue tenant key status %d: %s", status, body)
	}
	var resp tenantKeyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}
	return resp.Key, nil
}

func (r runner) createLease(ctx context.Context, tenantKey, idempotency string, requirePassword bool) (leaseResponse, error) {
	payload := map[string]any{"subject": map[string]any{"subject_type": "workload", "subject_id": idempotency}, "resource_ref": map[string]any{"kind": "url", "id": "https://example.com"}, "ttl_seconds": 120}
	status, body, err := r.request(ctx, http.MethodPost, "/v1/leases", tenantKey, idempotency, payload)
	if err != nil {
		return leaseResponse{}, err
	}
	if status != http.StatusCreated {
		return leaseResponse{}, fmt.Errorf("create lease status %d: %s", status, body)
	}
	var lease leaseResponse
	if err := json.Unmarshal(body, &lease); err != nil {
		return leaseResponse{}, err
	}
	if lease.LeaseID == "" || lease.ProxyID == "" || (requirePassword && lease.Password == "") {
		return leaseResponse{}, fmt.Errorf("incomplete lease response: %s", body)
	}
	return lease, nil
}

func (r runner) request(ctx context.Context, method, path, key, idempotency string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
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
			last = fmt.Sprintf("status %d: %s", status, body)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("ready timeout: %s", last)
}

func startDocker(ctx context.Context, cfg config) error {
	env := append(os.Environ(), "PROXYHARBOR_ADMIN_KEY="+cfg.AdminKey, "PROXYHARBOR_KEY_PEPPER="+cfg.KeyPepper)
	commands := [][]string{
		{"compose", "-f", cfg.ComposeFile, "down", "-v", "--remove-orphans"},
		{"build", "-t", "proxyharbor:ha-test", "."},
		{"compose", "-f", cfg.ComposeFile, "up", "-d", "--no-build", "--remove-orphans"},
	}
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

func firstErr(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func firstErrChan(errs <-chan error) error {
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func randomHex(bytesLen int) (string, error) {
	raw := make([]byte, bytesLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

var _ = errors.Is
