package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	proxyharbor "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"
)

type config struct {
	BaseURL        string
	AdminKey       string
	KeyPepper      string
	TenantKey      string
	Concurrency    int
	Samples        int
	DisableSamples int
	Docker         bool
	ComposeFile    string
	Timeout        time.Duration
}

type tenantKeyResponse struct {
	Key string `json:"key"`
}

func main() {
	cfg := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hasdkcheck:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.BaseURL, "base-url", envDefault("PROXYHARBOR_BASE_URL", "http://127.0.0.1:18081"), "ProxyHarbor HA load-balancer URL")
	flag.StringVar(&cfg.AdminKey, "admin-key", "", "admin key; defaults to PROXYHARBOR_ADMIN_KEY")
	flag.StringVar(&cfg.KeyPepper, "key-pepper", "", "key pepper used when -docker starts compose; defaults to PROXYHARBOR_KEY_PEPPER")
	flag.StringVar(&cfg.TenantKey, "tenant-key", "", "existing tenant key; defaults to PROXYHARBOR_TENANT_KEY; when empty the runner issues one with the admin key")
	flag.IntVar(&cfg.Concurrency, "concurrency", 96, "concurrent SDK GetProxy workers")
	flag.IntVar(&cfg.Samples, "samples", 2000, "SDK forced lease acquisitions for weighted distribution")
	flag.IntVar(&cfg.DisableSamples, "disable-samples", 300, "SDK forced lease acquisitions after disabling one proxy")
	flag.BoolVar(&cfg.Docker, "docker", false, "start docker-compose HA test topology")
	flag.StringVar(&cfg.ComposeFile, "compose-file", defaultComposeFile(), "compose file used with -docker")
	flag.DurationVar(&cfg.Timeout, "timeout", 10*time.Minute, "overall timeout")
	flag.Parse()
	if cfg.AdminKey == "" {
		cfg.AdminKey = os.Getenv("PROXYHARBOR_ADMIN_KEY")
	}
	if cfg.KeyPepper == "" {
		cfg.KeyPepper = os.Getenv("PROXYHARBOR_KEY_PEPPER")
	}
	if cfg.TenantKey == "" {
		cfg.TenantKey = os.Getenv("PROXYHARBOR_TENANT_KEY")
	}
	if cfg.Concurrency < 2 {
		cfg.Concurrency = 2
	}
	if cfg.Samples < 500 {
		cfg.Samples = 500
	}
	if cfg.DisableSamples < 100 {
		cfg.DisableSamples = 100
	}
	return cfg
}

func defaultComposeFile() string { return "docker-compose.ha-test.yaml" }

func projectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for _, candidate := range []string{cwd, filepath.Join(cwd, "..", "..")} {
		abs, absErr := filepath.Abs(candidate)
		if absErr != nil {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(abs, "Dockerfile")); statErr == nil {
			return abs
		}
	}
	return cwd
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
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if err := waitReady(ctx, baseURL); err != nil {
		return err
	}
	admin, err := proxyharbor.New(proxyharbor.WithBaseURL(baseURL), proxyharbor.WithAdminKey(cfg.AdminKey), proxyharbor.WithTimeout(20*time.Second))
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	if err := seedProxies(ctx, admin); err != nil {
		return err
	}
	if cfg.TenantKey == "" {
		tenantID := fmt.Sprintf("sdk-ha-%d", time.Now().UnixNano())
		key, err := issueTenantKey(ctx, baseURL, cfg.AdminKey, tenantID)
		if err != nil {
			return err
		}
		cfg.TenantKey = key
	}
	tenant, err := proxyharbor.New(
		proxyharbor.WithBaseURL(baseURL),
		proxyharbor.WithTenantKey(cfg.TenantKey),
		proxyharbor.WithDefaultTarget("https://example.com"),
		proxyharbor.WithTimeout(20*time.Second),
		proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{AutoRenew: false, AutoReacquire: false}),
	)
	if err != nil {
		return err
	}
	defer tenant.Close(ctx)

	started := time.Now()
	counts, err := getProxyMany(ctx, tenant, "sdk-dist", cfg.Samples, cfg.Concurrency)
	if err != nil {
		return err
	}
	if err := assertWeighted(counts, cfg.Samples); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok sdk weighted distribution       samples=%d concurrency=%d duration=%s counts=%s\n", cfg.Samples, cfg.Concurrency, time.Since(started).Round(time.Millisecond), formatCounts(counts))

	if _, err := admin.Proxies.Upsert(ctx, proxyharbor.ProxyDTO{ID: "sdk-ha-proxy-09", Endpoint: proxyEndpoint(9), Healthy: false, Weight: 10}); err != nil {
		return fmt.Errorf("disable proxy via sdk: %w", err)
	}
	started = time.Now()
	counts, err = getProxyMany(ctx, tenant, "sdk-disabled", cfg.DisableSamples, cfg.Concurrency)
	if err != nil {
		return err
	}
	if counts["sdk-ha-proxy-09"] != 0 {
		return fmt.Errorf("disabled proxy sdk-ha-proxy-09 received %d leases; counts=%s", counts["sdk-ha-proxy-09"], formatCounts(counts))
	}
	fmt.Fprintf(stdout, "ok sdk disabled proxy excluded    samples=%d concurrency=%d duration=%s counts=%s\n", cfg.DisableSamples, cfg.Concurrency, time.Since(started).Round(time.Millisecond), formatCounts(counts))
	if _, err := admin.Proxies.Upsert(ctx, proxyharbor.ProxyDTO{ID: "sdk-ha-proxy-09", Endpoint: proxyEndpoint(9), Healthy: true, Weight: 10}); err != nil {
		return err
	}
	return runSDKHAHappyPath(ctx, admin, tenant, stdout)
}

func runSDKHAHappyPath(ctx context.Context, admin, tenant *proxyharbor.Client, stdout io.Writer) (err error) {
	seedsIsolated := false
	var addedID string
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var cleanupErrs []string
		if addedID != "" {
			if cleanupErr := admin.Proxies.Delete(cleanupCtx, addedID); cleanupErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Sprintf("cleanup proxy %s: %v", addedID, cleanupErr))
			}
		}
		if seedsIsolated {
			if restoreErr := setSeededProxyHealth(cleanupCtx, admin, true); restoreErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Sprintf("restore seeded proxies: %v", restoreErr))
			}
		}
		if err == nil && len(cleanupErrs) > 0 {
			err = fmt.Errorf("sdk ha happy path cleanup: %s", strings.Join(cleanupErrs, "; "))
		}
	}()
	seedsIsolated = true
	if err := setSeededProxyHealth(ctx, admin, false); err != nil {
		return fmt.Errorf("sdk ha happy path prepare pool: %w", err)
	}

	endpoint := "http://198.51.100.10:28080"
	proxyID := "sdk-ha-flow-proxy-" + time.Now().UTC().Format("20060102150405.000000000")
	added, err := admin.Proxies.Upsert(ctx, proxyharbor.ProxyDTO{
		ID:       proxyID,
		Endpoint: endpoint,
		Healthy:  true,
		Weight:   1,
	})
	if err != nil {
		return fmt.Errorf("sdk ha happy path add proxy: %w", err)
	}
	if added.ID == "" {
		return fmt.Errorf("sdk ha happy path add proxy: missing proxy id for endpoint %s", endpoint)
	}
	addedID = added.ID

	key := "sdk-ha-flow-" + time.Now().UTC().Format("20060102150405.000000000")
	proxy, err := tenant.GetProxy(
		ctx,
		proxyharbor.WithKey(key),
		proxyharbor.WithSubjectID(key),
		proxyharbor.WithForceNew(),
		proxyharbor.WithTTL(2*time.Minute),
	)
	if err != nil {
		return fmt.Errorf("sdk ha happy path get proxy: %w", err)
	}
	if proxy.LeaseID == "" {
		return fmt.Errorf("sdk ha happy path get proxy: missing lease id for key %s", key)
	}
	if proxy.ProxyID == "" {
		return fmt.Errorf("sdk ha happy path get proxy: missing proxy id for lease %s", proxy.LeaseID)
	}
	if proxy.URL == "" {
		return fmt.Errorf("sdk ha happy path get proxy: empty proxy url for lease %s", proxy.LeaseID)
	}
	if proxy.ProxyID != added.ID {
		return fmt.Errorf("sdk ha happy path get proxy: leased proxy id %s, want added proxy %s", proxy.ProxyID, added.ID)
	}

	renewed, err := tenant.Leases.Renew(ctx, proxy.LeaseID)
	if err != nil {
		return fmt.Errorf("sdk ha happy path renew lease: %w", err)
	}
	if renewed.LeaseID != proxy.LeaseID {
		return fmt.Errorf("sdk ha happy path renew lease: lease id changed got=%s want=%s", renewed.LeaseID, proxy.LeaseID)
	}
	if renewed.ProxyID == "" {
		return fmt.Errorf("sdk ha happy path renew lease: missing proxy id for lease %s", proxy.LeaseID)
	}
	if renewed.ProxyID != added.ID {
		return fmt.Errorf("sdk ha happy path renew lease: proxy id changed got=%s want=%s", renewed.ProxyID, added.ID)
	}
	if !renewed.ExpiresAt.After(proxy.ExpiresAt) {
		return fmt.Errorf("sdk ha happy path renew lease: expiry did not move forward before=%s after=%s", proxy.ExpiresAt.UTC().Format(time.RFC3339), renewed.ExpiresAt.UTC().Format(time.RFC3339))
	}

	fmt.Fprintf(stdout, "ok sdk ha happy path              added_proxy=%s lease_id=%s proxy_id=%s renewed_until=%s\n",
		added.ID, proxy.LeaseID, proxy.ProxyID, renewed.ExpiresAt.UTC().Format(time.RFC3339))
	return nil
}

func setSeededProxyHealth(ctx context.Context, admin *proxyharbor.Client, healthy bool) error {
	for i := 0; i < 10; i++ {
		if _, err := admin.Proxies.Upsert(ctx, proxyharbor.ProxyDTO{
			ID:       fmt.Sprintf("sdk-ha-proxy-%02d", i),
			Endpoint: proxyEndpoint(i),
			Healthy:  healthy,
			Weight:   i + 1,
		}); err != nil {
			return fmt.Errorf("seeded proxy %02d healthy=%t: %w", i, healthy, err)
		}
	}
	return nil
}

func seedProxies(ctx context.Context, client *proxyharbor.Client) error {
	for i := 0; i < 10; i++ {
		_, err := client.Proxies.Upsert(ctx, proxyharbor.ProxyDTO{ID: fmt.Sprintf("sdk-ha-proxy-%02d", i), Endpoint: proxyEndpoint(i), Healthy: true, Weight: i + 1})
		if err != nil {
			return fmt.Errorf("seed sdk proxy %d: %w", i, err)
		}
	}
	return nil
}

func getProxyMany(ctx context.Context, client *proxyharbor.Client, prefix string, total, concurrency int) (map[string]int, error) {
	counts := map[string]int{}
	leases := make(chan proxyharbor.Proxy, total)
	errs := make(chan error, total)
	jobs := make(chan int, total)
	if concurrency > total {
		concurrency = total
	}
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for job := range jobs {
				key := fmt.Sprintf("%s-%d-%d-%d", prefix, worker, job, time.Now().UnixNano())
				proxy, err := client.GetProxy(ctx, proxyharbor.WithKey(key), proxyharbor.WithSubjectID(key), proxyharbor.WithForceNew(), proxyharbor.WithTTL(2*time.Minute))
				if err != nil {
					errs <- err
					continue
				}
				leases <- proxy
			}
		}(i)
	}
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	close(leases)
	for err := range errs {
		if err != nil {
			return nil, err
		}
	}
	for lease := range leases {
		counts[lease.ProxyID]++
	}
	seen := 0
	for _, count := range counts {
		seen += count
	}
	if seen != total {
		return nil, fmt.Errorf("created %d sdk leases, want %d; counts=%s", seen, total, formatCounts(counts))
	}
	return counts, nil
}

func assertWeighted(counts map[string]int, total int) error {
	totalWeight := 55.0
	for i := 0; i < 10; i++ {
		proxyID := fmt.Sprintf("sdk-ha-proxy-%02d", i)
		want := float64(i+1) / totalWeight * float64(total)
		got := float64(counts[proxyID])
		lower := math.Max(3, want*0.55)
		upper := want*1.55 + 3
		if got < lower || got > upper {
			return fmt.Errorf("sdk weighted distribution proxy=%s got=%.0f want about %.1f counts=%s", proxyID, got, want, formatCounts(counts))
		}
	}
	return nil
}

func issueTenantKey(ctx context.Context, baseURL, adminKey, tenantID string) (string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	if status, body, err := jsonRequest(ctx, client, http.MethodPost, baseURL+"/admin/tenants", adminKey, map[string]any{"id": tenantID, "display_name": "SDK HA"}); err != nil {
		return "", err
	} else if status != http.StatusCreated && status != http.StatusConflict {
		return "", fmt.Errorf("create sdk tenant status %d: %s", status, bodySummary(body))
	}
	status, body, err := jsonRequest(ctx, client, http.MethodPost, baseURL+"/admin/tenants/"+tenantID+"/keys", adminKey, map[string]any{"label": "sdk-ha", "purpose": "sdk_ha_check"})
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("issue sdk tenant key status %d: %s", status, bodySummary(body))
	}
	var resp tenantKeyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}
	if resp.Key == "" {
		return "", fmt.Errorf("tenant key missing from response: %s", bodySummary(body))
	}
	return resp.Key, nil
}

func jsonRequest(ctx context.Context, client *http.Client, method, url, key string, payload map[string]any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ProxyHarbor-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func waitReady(ctx context.Context, baseURL string) error {
	deadline := time.Now().Add(90 * time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	var last string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/readyz", nil)
		resp, err := client.Do(req)
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Sprintf("status=%d body=%s", resp.StatusCode, bodySummary(raw))
		} else {
			last = err.Error()
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("ready timeout: %s", last)
}

func startDocker(ctx context.Context, cfg config) error {
	root := projectRoot()
	composeFile := cfg.ComposeFile
	if !filepath.IsAbs(composeFile) {
		composeFile = filepath.Join(root, composeFile)
	}
	envFile, cleanup, err := composeEnvFile(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	commands := [][]string{{"compose", "-f", composeFile, "down", "-v", "--remove-orphans"}, {"build", "--pull=false", "-t", "proxyharbor:ha-test", "."}, {"compose", "-f", composeFile, "up", "-d", "--no-build", "--remove-orphans"}}
	for _, args := range commands {
		args = addComposeEnvFile(args, envFile)
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = root
		cmd.Env = scrubSecretEnv(os.Environ())
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, bodySummary(out))
		}
	}
	return nil
}

func cleanupDocker(cfg config, timeout time.Duration) {
	root := projectRoot()
	composeFile := cfg.ComposeFile
	if !filepath.IsAbs(composeFile) {
		composeFile = filepath.Join(root, composeFile)
	}
	envFile, cleanup, err := composeEnvFile(cfg)
	if err != nil {
		return
	}
	defer cleanup()
	args := addComposeEnvFile([]string{"compose", "-f", composeFile, "down", "-v", "--remove-orphans"}, envFile)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = root
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
	content := fmt.Sprintf("PROXYHARBOR_ADMIN_KEY=%s\nPROXYHARBOR_KEY_PEPPER=%s\n", envFileValue(cfg.AdminKey), envFileValue(cfg.KeyPepper))
	content += optionalComposeSecretEnv()
	_, writeErr := file.WriteString(content)
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

func optionalComposeSecretEnv() string {
	var out strings.Builder
	for _, key := range []string{"PROXYHARBOR_MYSQL_DSN", "PROXYHARBOR_REDIS_PASSWORD"} {
		if value := os.Getenv(key); value != "" {
			out.WriteString(key)
			out.WriteByte('=')
			out.WriteString(envFileValue(value))
			out.WriteByte('\n')
		}
	}
	return out.String()
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

func proxyEndpoint(i int) string { return fmt.Sprintf("http://127.0.0.1:%d", 19200+i) }

func formatCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
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
