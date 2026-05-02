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
	"math"
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
	BaseURL         string
	AdminKey        string
	KeyPepper       string
	TenantID        string
	Concurrency     int
	SelectorSamples int
	DisableSamples  int
	Docker          bool
	ComposeFile     string
	Timeout         time.Duration
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
	flag.StringVar(&cfg.BaseURL, "base-url", envDefault("PROXYHARBOR_BASE_URL", "http://127.0.0.1:18081"), "ProxyHarbor HA load-balancer URL")
	flag.StringVar(&cfg.AdminKey, "admin-key", "", "admin key; defaults to PROXYHARBOR_ADMIN_KEY")
	flag.StringVar(&cfg.KeyPepper, "key-pepper", "", "key pepper used when -docker starts compose; defaults to PROXYHARBOR_KEY_PEPPER")
	flag.StringVar(&cfg.TenantID, "tenant", "ha-correct", "tenant id")
	flag.IntVar(&cfg.Concurrency, "concurrency", 24, "concurrent requests per scenario")
	flag.IntVar(&cfg.SelectorSamples, "selector-samples", 500, "lease creates for zfair weight distribution check")
	flag.IntVar(&cfg.DisableSamples, "disable-samples", 100, "lease creates after disabling one proxy")
	flag.BoolVar(&cfg.Docker, "docker", false, "start docker-compose HA test topology")
	flag.StringVar(&cfg.ComposeFile, "compose-file", "docker-compose.ha-test.yaml", "compose file used with -docker")
	flag.DurationVar(&cfg.Timeout, "timeout", 4*time.Minute, "overall timeout")
	flag.Parse()
	if cfg.AdminKey == "" {
		cfg.AdminKey = os.Getenv("PROXYHARBOR_ADMIN_KEY")
	}
	if cfg.KeyPepper == "" {
		cfg.KeyPepper = os.Getenv("PROXYHARBOR_KEY_PEPPER")
	}
	if cfg.Concurrency < 2 {
		cfg.Concurrency = 2
	}
	if cfg.SelectorSamples < 100 {
		cfg.SelectorSamples = 100
	}
	if cfg.DisableSamples < 20 {
		cfg.DisableSamples = 20
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
			cleanupDocker(cfg, 90*time.Second)
			return err
		}
		defer cleanupDocker(cfg, 90*time.Second)
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
		{name: "zfair weighted distribution", fn: r.checkZFairWeightedDistribution},
		{name: "zfair disabled proxy excluded", fn: r.checkDisabledProxyExcluded},
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
	for i := 0; i < 10; i++ {
		if err := r.upsertProxy(ctx, fmt.Sprintf("ha-proxy-%02d", i), 1+i, true); err != nil {
			return err
		}
	}
	return nil
}

func (r runner) upsertProxy(ctx context.Context, id string, weight int, healthy bool) error {
	payload := map[string]any{"id": id, "endpoint": proxyEndpoint(id), "healthy": healthy, "weight": weight, "health_score": 100}
	status, body, err := r.request(ctx, http.MethodPost, "/v1/proxies", r.cfg.AdminKey, "", payload)
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		return nil
	}
	if status != http.StatusConflict {
		return fmt.Errorf("seed proxy %s status %d: %s", id, status, bodySummary(body))
	}
	status, body, err = r.request(ctx, http.MethodPut, "/v1/proxies/"+url.PathEscape(id), r.cfg.AdminKey, "", payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("update proxy %s status %d: %s", id, status, bodySummary(body))
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

func (r runner) checkZFairWeightedDistribution(ctx context.Context) error {
	tenantKey, err := r.issueTenantKey(ctx, "zfair-distribution")
	if err != nil {
		return err
	}
	counts, err := r.createManyLeases(ctx, tenantKey, "zfair-dist", r.cfg.SelectorSamples)
	if err != nil {
		return err
	}
	totalWeight := 55.0
	for i := 0; i < 10; i++ {
		proxyID := fmt.Sprintf("ha-proxy-%02d", i)
		want := float64(i+1) / totalWeight * float64(r.cfg.SelectorSamples)
		got := float64(counts[proxyID])
		lower := math.Max(1, want*0.45)
		upper := want*1.75 + 2
		if got < lower || got > upper {
			return fmt.Errorf("weighted distribution proxy=%s got=%.0f want≈%.1f counts=%v", proxyID, got, want, counts)
		}
	}
	return nil
}

func (r runner) checkDisabledProxyExcluded(ctx context.Context) error {
	proxyID := "ha-proxy-09"
	if err := r.upsertProxy(ctx, proxyID, 10, false); err != nil {
		return err
	}
	tenantKey, err := r.issueTenantKey(ctx, "zfair-disabled")
	if err != nil {
		return err
	}
	counts, err := r.createManyLeases(ctx, tenantKey, "zfair-disabled", r.cfg.DisableSamples)
	if err != nil {
		return err
	}
	if counts[proxyID] != 0 {
		return fmt.Errorf("disabled proxy %s received %d new leases; counts=%v", proxyID, counts[proxyID], counts)
	}
	return r.upsertProxy(ctx, proxyID, 10, true)
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
			errs <- fmt.Errorf("revoke status %d: %s", status, bodySummary(body))
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
				errs <- fmt.Errorf("renew status %d: %s", status, bodySummary(body))
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
		return fmt.Errorf("renew after revoke succeeded; body=%s renewOKDuringRace=%d conflicts=%d", bodySummary(body), renewOK.Load(), conflictOrGone.Load())
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
		return fmt.Errorf("create disabled-test tenant status %d: %s", status, bodySummary(body))
	}
	status, body, err = r.request(ctx, http.MethodPatch, "/admin/tenants/"+url.PathEscape(tenantID), r.cfg.AdminKey, "", map[string]any{"status": "disabled"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("disable tenant status %d: %s", status, bodySummary(body))
	}
	status, body, err = r.request(ctx, http.MethodPost, "/admin/tenants/"+url.PathEscape(tenantID)+"/keys", r.cfg.AdminKey, "", map[string]any{"label": "after-disable"})
	if err != nil {
		return err
	}
	if status != http.StatusForbidden {
		return fmt.Errorf("create key for disabled tenant status %d, want 403: %s", status, bodySummary(body))
	}
	var errBody errorResponse
	_ = json.Unmarshal(body, &errBody)
	if errBody.Error != "tenant_disabled" {
		return fmt.Errorf("disabled tenant error = %q, want tenant_disabled; body=%s", errBody.Error, bodySummary(body))
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

func (r runner) createManyLeases(ctx context.Context, tenantKey, idemPrefix string, total int) (map[string]int, error) {
	counts := map[string]int{}
	errCh := make(chan error, total)
	leaseCh := make(chan leaseResponse, total)
	workers := r.cfg.Concurrency
	if workers > total {
		workers = total
	}
	jobs := make(chan int, total)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				lease, err := r.createLease(ctx, tenantKey, fmt.Sprintf("%s-%d-%d", idemPrefix, time.Now().UnixNano(), job), true)
				if err != nil {
					errCh <- err
					continue
				}
				leaseCh <- lease
			}
		}()
	}
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errCh)
	close(leaseCh)
	if err := firstErrChan(errCh); err != nil {
		return nil, err
	}
	for lease := range leaseCh {
		counts[lease.ProxyID]++
	}
	if got := len(leaseCh); got != 0 {
		return nil, fmt.Errorf("internal lease channel not drained: %d", got)
	}
	seen := 0
	for _, count := range counts {
		seen += count
	}
	if seen != total {
		return nil, fmt.Errorf("created %d leases, want %d; counts=%v", seen, total, counts)
	}
	return counts, nil
}

func (r runner) issueTenantKey(ctx context.Context, label string) (string, error) {
	status, body, err := r.request(ctx, http.MethodPost, "/admin/tenants/"+url.PathEscape(r.cfg.TenantID)+"/keys", r.cfg.AdminKey, "", map[string]any{"label": label, "purpose": "ha_correctness"})
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("issue tenant key status %d: %s", status, bodySummary(body))
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
		return leaseResponse{}, fmt.Errorf("create lease status %d: %s", status, bodySummary(body))
	}
	var lease leaseResponse
	if err := json.Unmarshal(body, &lease); err != nil {
		return leaseResponse{}, err
	}
	if lease.LeaseID == "" || lease.ProxyID == "" || (requirePassword && lease.Password == "") {
		return leaseResponse{}, fmt.Errorf("incomplete lease response: %s", bodySummary(body))
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
			last = fmt.Sprintf("status %d: %s", status, bodySummary(body))
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
	envFile, cleanup, err := composeEnvFile(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	commands := [][]string{
		{"compose", "-f", cfg.ComposeFile, "down", "-v", "--remove-orphans"},
		{"build", "--pull=false", "-t", "proxyharbor:ha-test", "."},
		{"compose", "-f", cfg.ComposeFile, "up", "-d", "--no-build", "--remove-orphans"},
	}
	for _, args := range commands {
		args = addComposeEnvFile(args, envFile)
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Env = scrubSecretEnv(os.Environ())
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, bodySummary(out))
		}
	}
	return nil
}

func cleanupDocker(cfg config, timeout time.Duration) {
	envFile, cleanup, err := composeEnvFile(cfg)
	if err != nil {
		return
	}
	defer cleanup()
	args := addComposeEnvFile([]string{"compose", "-f", cfg.ComposeFile, "down", "-v", "--remove-orphans"}, envFile)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = scrubSecretEnv(os.Environ())
	_ = cmd.Run()
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

func addComposeEnvFile(args []string, envFile string) []string {
	if len(args) > 0 && args[0] == "compose" {
		out := []string{"compose", "--env-file", envFile}
		out = append(out, args[1:]...)
		return out
	}
	return args
}

func composeEnvFile(cfg config) (string, func(), error) {
	file, err := os.CreateTemp("", "proxyharbor-compose-*.env")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	_, writeErr := fmt.Fprintf(file, "PROXYHARBOR_ADMIN_KEY=%s\nPROXYHARBOR_KEY_PEPPER=%s\n", envFileValue(cfg.AdminKey), envFileValue(cfg.KeyPepper))
	closeErr := file.Close()
	if writeErr != nil {
		cleanup()
		return "", func() {}, writeErr
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, closeErr
	}
	return path, cleanup, nil
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

func proxyEndpoint(id string) string {
	_, suffix, ok := strings.Cut(id, "ha-proxy-")
	if !ok {
		return "http://127.0.0.1:19100"
	}
	var index int
	_, _ = fmt.Sscanf(suffix, "%02d", &index)
	return fmt.Sprintf("http://127.0.0.1:%d", 19100+index)
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
