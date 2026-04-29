package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAdminKey = "dev-admin-key-min-32-chars-long!!!!"
	defaultPepper   = "dev-key-pepper-min-32-bytes-random!!!!"
)

type config struct {
	BaseURL      string
	AdminKey     string
	TenantID     string
	ProxyCount   int
	Requests     int
	Concurrency  int
	Operation    string
	Output       string
	OutPath      string
	StartDocker  bool
	ComposeFile  string
	HostPort     string
	WarmupLeases int
	Timeout      time.Duration
}

type benchmarkSummary struct {
	Operation          string         `json:"operation"`
	Total              int            `json:"total"`
	Success            int            `json:"success"`
	Failure            int            `json:"failure"`
	ElapsedMS          float64        `json:"elapsed_ms"`
	RPS                float64        `json:"rps"`
	P50MS              float64        `json:"p50_ms"`
	P90MS              float64        `json:"p90_ms"`
	P95MS              float64        `json:"p95_ms"`
	P99MS              float64        `json:"p99_ms"`
	MaxMS              float64        `json:"max_ms"`
	StatusDistribution map[int]int    `json:"status_distribution"`
	ProxyDistribution  map[string]int `json:"proxy_distribution"`
	StartedAt          time.Time      `json:"started_at"`
	FinishedAt         time.Time      `json:"finished_at"`
}

type operationResult struct {
	Operation string
	Status    int
	ProxyID   string
	Latency   time.Duration
	Success   bool
	Error     string
}

type leaseInfo struct {
	LeaseID  string `json:"lease_id"`
	Password string `json:"password"`
	ProxyID  string `json:"proxy_id"`
}

func main() {
	cfg := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "singlebench:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.BaseURL, "base-url", envDefault("PROXYHARBOR_BASE_URL", "http://localhost:18080"), "ProxyHarbor base URL")
	flag.StringVar(&cfg.AdminKey, "admin-key", envDefault("PROXYHARBOR_ADMIN_KEY", defaultAdminKey), "admin key for setup and admin endpoints")
	flag.StringVar(&cfg.TenantID, "tenant", "bench-tenant", "tenant id used by benchmark")
	flag.IntVar(&cfg.ProxyCount, "proxies", 8, "number of proxies to seed")
	flag.IntVar(&cfg.Requests, "requests", 200, "number of measured requests")
	flag.IntVar(&cfg.Concurrency, "concurrency", 16, "concurrent workers")
	flag.StringVar(&cfg.Operation, "operation", "mixed", "operation: lease_create, renew, validate, catalog, mixed")
	flag.StringVar(&cfg.Output, "output", "json", "output format: json or csv")
	flag.StringVar(&cfg.OutPath, "out", "", "optional output file path")
	flag.BoolVar(&cfg.StartDocker, "docker", false, "start docker compose single profile and wait for readiness")
	flag.StringVar(&cfg.ComposeFile, "compose-file", "docker-compose.yaml", "compose file for -docker")
	flag.StringVar(&cfg.HostPort, "host-port", "18080", "host port for -docker")
	flag.IntVar(&cfg.WarmupLeases, "warmup-leases", 32, "leases to pre-create for renew/validate/mixed")
	flag.DurationVar(&cfg.Timeout, "timeout", 2*time.Minute, "overall timeout")
	flag.Parse()
	if cfg.Requests < 1 {
		cfg.Requests = 1
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.ProxyCount < 1 {
		cfg.ProxyCount = 1
	}
	return cfg
}

func run(ctx context.Context, cfg config, stdout io.Writer) error {
	if cfg.StartDocker {
		if err := startDocker(ctx, cfg); err != nil {
			return err
		}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	if err := waitReady(ctx, client, cfg.BaseURL); err != nil {
		return err
	}
	tenantKey, err := prepare(ctx, client, cfg)
	if err != nil {
		return err
	}
	warmups, err := createWarmupLeases(ctx, client, cfg, tenantKey)
	if err != nil {
		return err
	}
	results, elapsed := exercise(ctx, client, cfg, tenantKey, warmups)
	summary := summarize(cfg.Operation, results, elapsed)
	out := stdout
	if cfg.OutPath != "" {
		file, err := os.Create(cfg.OutPath)
		if err != nil {
			return err
		}
		defer file.Close()
		out = file
	}
	if strings.EqualFold(cfg.Output, "csv") {
		return writeCSVSummary(out, summary)
	}
	return writeJSONSummary(out, summary)
}

func startDocker(ctx context.Context, cfg config) error {
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", cfg.ComposeFile, "up", "-d", "--build", "--pull", "never")
	cmd.Env = append(os.Environ(),
		"PROXYHARBOR_HOST_PORT="+cfg.HostPort,
		"PROXYHARBOR_ADMIN_KEY="+cfg.AdminKey,
		"PROXYHARBOR_KEY_PEPPER="+envDefault("PROXYHARBOR_KEY_PEPPER", defaultPepper),
		"PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true",
		"PROXYHARBOR_AUTH_REFRESH_INTERVAL=1s",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitReady(ctx context.Context, client *http.Client, baseURL string) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/readyz", nil)
		resp, err := client.Do(req)
		if err == nil && resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("readyz status %d", resp.StatusCode)
		} else if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("readyz timeout")
	}
	return lastErr
}

func prepare(ctx context.Context, client *http.Client, cfg config) (string, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	_, _ = requestJSON(ctx, client, http.MethodPost, base+"/admin/tenants", cfg.AdminKey, "", map[string]any{"id": cfg.TenantID, "display_name": "Benchmark Tenant"})
	var keyResp struct {
		Key string `json:"key"`
	}
	body, err := requestJSON(ctx, client, http.MethodPost, base+"/admin/tenants/"+url.PathEscape(cfg.TenantID)+"/keys", cfg.AdminKey, "", map[string]any{"label": "singlebench", "purpose": "performance"})
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, &keyResp); err != nil {
		return "", err
	}
	for i := 0; i < cfg.ProxyCount; i++ {
		proxy := map[string]any{"id": fmt.Sprintf("bench-proxy-%03d", i), "endpoint": fmt.Sprintf("http://127.0.0.1:%d", 19000+i), "healthy": true, "weight": 1 + (i % 10), "health_score": 100}
		if _, err := requestJSON(ctx, client, http.MethodPost, base+"/v1/proxies", cfg.AdminKey, "", proxy); err != nil {
			return "", err
		}
	}
	return keyResp.Key, nil
}

func createWarmupLeases(ctx context.Context, client *http.Client, cfg config, tenantKey string) ([]leaseInfo, error) {
	count := cfg.WarmupLeases
	if count < cfg.Concurrency {
		count = cfg.Concurrency
	}
	leases := make([]leaseInfo, 0, count)
	for i := 0; i < count; i++ {
		lease, err := createLeaseWithRetry(ctx, client, cfg, tenantKey, fmt.Sprintf("warmup-%d-%d", time.Now().UnixNano(), i))
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

func createLeaseWithRetry(ctx context.Context, client *http.Client, cfg config, tenantKey, idempotency string) (leaseInfo, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		lease, err := createLease(ctx, client, cfg, tenantKey, idempotency)
		if err == nil {
			return lease, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return leaseInfo{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return leaseInfo{}, lastErr
}

func exercise(ctx context.Context, client *http.Client, cfg config, tenantKey string, warmups []leaseInfo) ([]operationResult, time.Duration) {
	started := time.Now()
	results := make([]operationResult, cfg.Requests)
	jobs := make(chan int)
	var idem atomic.Int64
	var wg sync.WaitGroup
	for worker := 0; worker < cfg.Concurrency; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			for i := range jobs {
				operation := pickOperation(cfg.Operation, i)
				lease := warmups[rng.Intn(len(warmups))]
				switch operation {
				case "lease_create":
					results[i] = measure(operation, func() (int, string, error) {
						created, err := createLease(ctx, client, cfg, tenantKey, fmt.Sprintf("run-%d", idem.Add(1)))
						return statusFromErr(err, http.StatusCreated), created.ProxyID, err
					})
				case "renew":
					results[i] = measure(operation, func() (int, string, error) { return renewLease(ctx, client, cfg, tenantKey, lease) })
				case "validate":
					results[i] = measure(operation, func() (int, string, error) { return validateLease(ctx, client, cfg, lease) })
				case "catalog":
					results[i] = measure(operation, func() (int, string, error) { return catalog(ctx, client, cfg) })
				default:
					results[i] = measure("mixed", func() (int, string, error) { return catalog(ctx, client, cfg) })
				}
			}
		}(worker)
	}
	for i := 0; i < cfg.Requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results, time.Since(started)
}

func pickOperation(operation string, i int) string {
	operation = strings.ToLower(operation)
	if operation != "mixed" {
		return operation
	}
	switch i % 10 {
	case 0, 1, 2:
		return "lease_create"
	case 3, 4:
		return "renew"
	case 5, 6, 7, 8:
		return "validate"
	default:
		return "catalog"
	}
}

func measure(operation string, fn func() (int, string, error)) operationResult {
	started := time.Now()
	status, proxyID, err := fn()
	latency := time.Since(started)
	result := operationResult{Operation: operation, Status: status, ProxyID: proxyID, Latency: latency, Success: err == nil && status >= 200 && status < 300}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func createLease(ctx context.Context, client *http.Client, cfg config, tenantKey, idempotency string) (leaseInfo, error) {
	payload := map[string]any{"subject": map[string]any{"subject_type": "workload", "subject_id": idempotency}, "resource_ref": map[string]any{"kind": "url", "id": "https://example.com"}}
	body, status, err := requestJSONStatus(ctx, client, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/v1/leases", tenantKey, idempotency, payload)
	if err != nil {
		return leaseInfo{}, err
	}
	if status < 200 || status >= 300 {
		return leaseInfo{}, fmt.Errorf("lease create status %d: %s", status, body)
	}
	var lease leaseInfo
	if err := json.Unmarshal(body, &lease); err != nil {
		return leaseInfo{}, err
	}
	return lease, nil
}

func renewLease(ctx context.Context, client *http.Client, cfg config, tenantKey string, lease leaseInfo) (int, string, error) {
	_, status, err := requestJSONStatus(ctx, client, http.MethodPost, strings.TrimRight(cfg.BaseURL, "/")+"/v1/leases/"+url.PathEscape(lease.LeaseID)+":renew", tenantKey, "", nil)
	return status, lease.ProxyID, err
}

func validateLease(ctx context.Context, client *http.Client, cfg config, lease leaseInfo) (int, string, error) {
	endpoint, _ := url.Parse(strings.TrimRight(cfg.BaseURL, "/") + "/v1/gateway/validate")
	query := endpoint.Query()
	query.Set("tenant_id", cfg.TenantID)
	query.Set("lease_id", lease.LeaseID)
	query.Set("password", lease.Password)
	query.Set("target", "https://example.com")
	endpoint.RawQuery = query.Encode()
	_, status, err := requestJSONStatus(ctx, client, http.MethodGet, endpoint.String(), cfg.AdminKey, "", nil)
	return status, lease.ProxyID, err
}

func catalog(ctx context.Context, client *http.Client, cfg config) (int, string, error) {
	_, status, err := requestJSONStatus(ctx, client, http.MethodGet, strings.TrimRight(cfg.BaseURL, "/")+"/v1/catalog/latest", cfg.AdminKey, "", nil)
	return status, "", err
}

func requestJSON(ctx context.Context, client *http.Client, method, endpoint, key, idempotency string, payload any) ([]byte, error) {
	body, status, err := requestJSONStatus(ctx, client, method, endpoint, key, idempotency, payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s %s status %d: %s", method, endpoint, status, string(body))
	}
	return body, nil
}

func requestJSONStatus(ctx context.Context, client *http.Client, method, endpoint, key, idempotency string, payload any) ([]byte, int, error) {
	var reader io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("ProxyHarbor-Key", key)
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 500 {
		return body, resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, resp.StatusCode, nil
}

func statusFromErr(err error, successStatus int) int {
	if err == nil {
		return successStatus
	}
	return 0
}

func summarize(operation string, results []operationResult, elapsed time.Duration) benchmarkSummary {
	latencies := make([]float64, 0, len(results))
	statusDist := map[int]int{}
	proxyDist := map[string]int{}
	success := 0
	for _, result := range results {
		latencies = append(latencies, float64(result.Latency.Milliseconds()))
		statusDist[result.Status]++
		if result.ProxyID != "" {
			proxyDist[result.ProxyID]++
		}
		if result.Success {
			success++
		}
	}
	sort.Float64s(latencies)
	elapsedMS := float64(elapsed.Milliseconds())
	if elapsedMS == 0 && len(results) > 0 {
		elapsedMS = 1
	}
	finished := time.Now().UTC()
	return benchmarkSummary{
		Operation:          operation,
		Total:              len(results),
		Success:            success,
		Failure:            len(results) - success,
		ElapsedMS:          elapsedMS,
		RPS:                round2(float64(len(results)) / elapsed.Seconds()),
		P50MS:              percentile(latencies, 0.50),
		P90MS:              percentile(latencies, 0.90),
		P95MS:              percentile(latencies, 0.95),
		P99MS:              percentile(latencies, 0.99),
		MaxMS:              percentile(latencies, 1),
		StatusDistribution: statusDist,
		ProxyDistribution:  proxyDist,
		StartedAt:          finished.Add(-elapsed),
		FinishedAt:         finished,
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func writeJSONSummary(w io.Writer, summary benchmarkSummary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(summary)
}

func writeCSVSummary(w io.Writer, summary benchmarkSummary) error {
	writer := csv.NewWriter(w)
	defer writer.Flush()
	if err := writer.Write([]string{"operation", "total", "success", "failure", "elapsed_ms", "rps", "p50_ms", "p90_ms", "p95_ms", "p99_ms", "max_ms", "started_at", "finished_at", "status_distribution", "proxy_distribution"}); err != nil {
		return err
	}
	if err := writer.Write([]string{summary.Operation, strconv.Itoa(summary.Total), strconv.Itoa(summary.Success), strconv.Itoa(summary.Failure), formatFloat(summary.ElapsedMS), formatFloat(summary.RPS), formatFloat(summary.P50MS), formatFloat(summary.P90MS), formatFloat(summary.P95MS), formatFloat(summary.P99MS), formatFloat(summary.MaxMS), summary.StartedAt.Format(time.RFC3339Nano), summary.FinishedAt.Format(time.RFC3339Nano), formatIntMap(summary.StatusDistribution), formatStringMap(summary.ProxyDistribution)}); err != nil {
		return err
	}
	return writer.Error()
}

func formatIntMap(m map[int]int) string {
	keys := make([]int, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d=%d", key, m[key]))
	}
	return strings.Join(parts, ";")
}

func formatStringMap(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, m[key]))
	}
	return strings.Join(parts, ";")
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
func round2(v float64) float64     { return math.Round(v*100) / 100 }
func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
