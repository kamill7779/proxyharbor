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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	opValidate    = "gateway_validate"
	opLeaseCreate = "lease_create"
	opLeaseRenew  = "lease_renew"

	defaultTarget         = "https://example.com"
	seedProxyCount        = 10
	requiredSoakCountHint = 500
)

var orderedOperations = []string{opValidate, opLeaseCreate, opLeaseRenew}

type config struct {
	BaseURL         string
	AdminKey        string
	KeyPepper       string
	TenantID        string
	TenantKey       string
	Mode            string
	Concurrency     int
	SamplesPerOp    int
	WarmupLeases    int
	Duration        time.Duration
	CreateTTL       time.Duration
	Docker          bool
	DockerSkipBuild bool
	ComposeFile     string
	Timeout         time.Duration
}

type runner struct {
	cfg            config
	client         *http.Client
	base           string
	tenantKey      string
	validateLeases []leaseInfo
	renewLeases    []leaseInfo
	ids            atomic.Uint64
}

type tenantKeyResponse struct {
	Key string `json:"key"`
}

type leaseInfo struct {
	LeaseID  string `json:"lease_id"`
	Password string `json:"password"`
	ProxyID  string `json:"proxy_id"`
}

type operationResult struct {
	Operation string
	Status    int
	Latency   time.Duration
	Success   bool
}

type reportMeta struct {
	Mode         string
	BaseURL      string
	Concurrency  int
	SamplesPerOp int
	Elapsed      time.Duration
	StartedAt    time.Time
	FinishedAt   time.Time
}

type pressureReport struct {
	Mode               string                     `json:"mode"`
	BaseURL            string                     `json:"base_url"`
	Concurrency        int                        `json:"concurrency"`
	SamplesPerOp       int                        `json:"samples_per_op,omitempty"`
	Total              int                        `json:"total"`
	Success            int                        `json:"success"`
	Failure            int                        `json:"failure"`
	OverallErrorRate   float64                    `json:"overall_error_rate"`
	ElapsedMS          float64                    `json:"elapsed_ms"`
	StartedAt          time.Time                  `json:"started_at"`
	FinishedAt         time.Time                  `json:"finished_at"`
	StatusDistribution map[int]int                `json:"status_distribution"`
	Operations         map[string]operationReport `json:"operations"`
	SoakThreshold      soakThresholdResult        `json:"soak_threshold"`
	Passed             bool                       `json:"passed"`
}

type operationReport struct {
	Total              int                     `json:"total"`
	Success            int                     `json:"success"`
	Failure            int                     `json:"failure"`
	ErrorRate          float64                 `json:"error_rate"`
	P50MS              float64                 `json:"p50_ms"`
	P95MS              float64                 `json:"p95_ms"`
	P99MS              float64                 `json:"p99_ms"`
	MaxMS              float64                 `json:"max_ms"`
	StatusDistribution map[int]int             `json:"status_distribution"`
	Threshold          operationThresholdCheck `json:"threshold"`
}

type operationThresholdCheck struct {
	TargetP95MS  float64  `json:"target_p95_ms"`
	TargetP99MS  float64  `json:"target_p99_ms"`
	MaxErrorRate float64  `json:"max_error_rate"`
	Pass         bool     `json:"pass"`
	Violations   []string `json:"violations,omitempty"`
}

type soakThresholdResult struct {
	RequiredConcurrency int      `json:"required_concurrency"`
	RequiredDurationMS  float64  `json:"required_duration_ms"`
	MaxErrorRate        float64  `json:"max_error_rate"`
	Pass                bool     `json:"pass"`
	Skipped             bool     `json:"skipped,omitempty"`
	Violations          []string `json:"violations,omitempty"`
}

type operationAccumulator struct {
	Total    int
	Success  int
	Failure  int
	Statuses map[int]int
	Latency  []float64
}

type accumulator struct {
	mu         sync.Mutex
	total      int
	success    int
	failure    int
	statuses   map[int]int
	operations map[string]*operationAccumulator
}

func main() {
	cfg := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hapressure:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.BaseURL, "base-url", envDefault("PROXYHARBOR_BASE_URL", "http://127.0.0.1:18081"), "ProxyHarbor HA load-balancer URL")
	flag.StringVar(&cfg.AdminKey, "admin-key", "", "admin key; defaults to PROXYHARBOR_ADMIN_KEY")
	flag.StringVar(&cfg.KeyPepper, "key-pepper", "", "key pepper used when -docker starts compose; defaults to PROXYHARBOR_KEY_PEPPER")
	flag.StringVar(&cfg.TenantID, "tenant", "ha-pressure", "tenant id used for pressure traffic")
	flag.StringVar(&cfg.TenantKey, "tenant-key", "", "existing tenant key; defaults to PROXYHARBOR_TENANT_KEY; when empty a key is issued with the admin key")
	flag.StringVar(&cfg.Mode, "mode", "pressure", "workload mode: pressure or soak")
	flag.IntVar(&cfg.Concurrency, "concurrency", 64, "concurrent workers")
	flag.IntVar(&cfg.SamplesPerOp, "samples-per-op", 200, "samples per operation in pressure mode")
	flag.IntVar(&cfg.WarmupLeases, "warmup-leases", 128, "warmup leases kept for validate/renew pools")
	flag.DurationVar(&cfg.Duration, "duration", 10*time.Minute, "run duration in soak mode")
	flag.DurationVar(&cfg.CreateTTL, "create-ttl", 2*time.Minute, "ttl for measured lease create requests")
	flag.BoolVar(&cfg.Docker, "docker", false, "start docker-compose HA test topology")
	flag.BoolVar(&cfg.DockerSkipBuild, "docker-skip-build", false, "reuse existing proxyharbor:ha-test image when starting docker HA topology")
	flag.StringVar(&cfg.ComposeFile, "compose-file", defaultComposeFile(), "compose file used with -docker")
	flag.DurationVar(&cfg.Timeout, "timeout", 20*time.Minute, "overall timeout")
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
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.SamplesPerOp < 1 {
		cfg.SamplesPerOp = 1
	}
	if cfg.WarmupLeases < 1 {
		cfg.WarmupLeases = 1
	}
	if cfg.CreateTTL < time.Second {
		cfg.CreateTTL = 2 * time.Minute
	}
	return cfg
}

func defaultComposeFile() string { return "docker-compose.ha-test.yaml" }

func run(ctx context.Context, cfg config, stdout io.Writer) error {
	var err error
	if cfg.AdminKey == "" {
		if !cfg.Docker {
			return errors.New("admin key required: set --admin-key or PROXYHARBOR_ADMIN_KEY")
		}
		cfg.AdminKey, err = randomHex(32)
		if err != nil {
			return err
		}
	}
	if cfg.KeyPepper == "" && cfg.Docker {
		cfg.KeyPepper, err = randomHex(32)
		if err != nil {
			return err
		}
	}
	if cfg.Docker {
		cleanupCompose(cfg, 90*time.Second, "down", "-v", "--remove-orphans")
		if err := startDocker(ctx, cfg); err != nil {
			cleanupCompose(cfg, 90*time.Second, "down", "-v", "--remove-orphans")
			return err
		}
		defer cleanupCompose(cfg, 90*time.Second, "down", "-v", "--remove-orphans")
	}
	r := &runner{
		cfg:    cfg,
		client: localHTTPClient(15 * time.Second),
		base:   strings.TrimRight(cfg.BaseURL, "/"),
	}
	if err := r.prepare(ctx); err != nil {
		return err
	}
	started := time.Now().UTC()
	acc := newAccumulator()
	switch strings.ToLower(cfg.Mode) {
	case "pressure":
		if err := r.runPressure(ctx, acc); err != nil {
			return err
		}
	case "soak":
		if err := r.runSoak(ctx, acc); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported mode %q", cfg.Mode)
	}
	finished := time.Now().UTC()
	meta := reportMeta{
		Mode:         strings.ToLower(cfg.Mode),
		BaseURL:      r.base,
		Concurrency:  cfg.Concurrency,
		SamplesPerOp: cfg.SamplesPerOp,
		Elapsed:      finished.Sub(started),
		StartedAt:    started,
		FinishedAt:   finished,
	}
	return writeJSONReport(stdout, acc.Report(meta))
}

func (r *runner) prepare(ctx context.Context) error {
	if err := r.waitReady(ctx); err != nil {
		return err
	}
	if err := r.ensureTenant(ctx); err != nil {
		return err
	}
	if err := r.seedProxies(ctx); err != nil {
		return err
	}
	key, err := r.ensureTenantKey(ctx)
	if err != nil {
		return err
	}
	r.tenantKey = key
	validateTTL := 15 * time.Minute
	if strings.EqualFold(r.cfg.Mode, "soak") && r.cfg.Duration+5*time.Minute > validateTTL {
		validateTTL = r.cfg.Duration + 5*time.Minute
	}
	poolSize := r.cfg.WarmupLeases
	if poolSize < r.cfg.Concurrency {
		poolSize = r.cfg.Concurrency
	}
	r.validateLeases, err = r.seedLeasePool(ctx, "validate", poolSize, validateTTL)
	if err != nil {
		return err
	}
	r.renewLeases, err = r.seedLeasePool(ctx, "renew", poolSize, validateTTL)
	if err != nil {
		return err
	}
	return nil
}

func (r *runner) runPressure(ctx context.Context, acc *accumulator) error {
	type job struct {
		Operation string
		Sequence  int
	}
	jobs := make(chan job, r.cfg.SamplesPerOp*len(orderedOperations))
	for sample := 0; sample < r.cfg.SamplesPerOp; sample++ {
		for _, operation := range orderedOperations {
			jobs <- job{Operation: operation, Sequence: sample}
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	for worker := 0; worker < r.cfg.Concurrency; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				acc.Add(r.executeOperation(ctx, j.Operation, worker, j.Sequence))
			}
		}(worker)
	}
	workers.Wait()
	return ctx.Err()
}

func (r *runner) runSoak(ctx context.Context, acc *accumulator) error {
	deadline := time.Now().Add(r.cfg.Duration)
	var workers sync.WaitGroup
	var counter atomic.Uint64
	for worker := 0; worker < r.cfg.Concurrency; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for {
				if ctx.Err() != nil || time.Now().After(deadline) {
					return
				}
				sequence := int(counter.Add(1)) - 1
				operation := orderedOperations[sequence%len(orderedOperations)]
				acc.Add(r.executeOperation(ctx, operation, worker, sequence))
			}
		}(worker)
	}
	workers.Wait()
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}

func (r *runner) executeOperation(ctx context.Context, operation string, worker, sequence int) operationResult {
	started := time.Now()
	status, err := r.performOperation(ctx, operation, worker, sequence)
	latency := time.Since(started)
	return operationResult{
		Operation: operation,
		Status:    status,
		Latency:   latency,
		Success:   err == nil && status >= 200 && status < 300,
	}
}

func (r *runner) performOperation(ctx context.Context, operation string, worker, sequence int) (int, error) {
	switch operation {
	case opValidate:
		lease := r.validateLeases[sequence%len(r.validateLeases)]
		return r.validateLease(ctx, lease)
	case opLeaseCreate:
		return r.createLease(ctx, r.uniqueKey("create", worker, sequence), r.cfg.CreateTTL)
	case opLeaseRenew:
		lease := r.renewLeases[sequence%len(r.renewLeases)]
		return r.renewLease(ctx, lease)
	default:
		return 0, fmt.Errorf("unsupported operation %q", operation)
	}
}

func (r *runner) ensureTenant(ctx context.Context) error {
	status, body, err := r.request(ctx, http.MethodPost, "/admin/tenants", r.cfg.AdminKey, "", map[string]any{
		"id":           r.cfg.TenantID,
		"display_name": "HA Pressure",
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusConflict {
		return fmt.Errorf("ensure tenant status %d: %s", status, bodySummary(body))
	}
	return nil
}

func (r *runner) seedProxies(ctx context.Context) error {
	for i := 0; i < seedProxyCount; i++ {
		id := fmt.Sprintf("ha-pressure-proxy-%02d", i)
		payload := map[string]any{
			"id":           id,
			"endpoint":     fmt.Sprintf("http://127.0.0.1:%d", 19300+i),
			"healthy":      true,
			"weight":       i + 1,
			"health_score": 100,
		}
		status, body, err := r.request(ctx, http.MethodPost, "/v1/proxies", r.cfg.AdminKey, "", payload)
		if err != nil {
			return err
		}
		if status == http.StatusCreated {
			continue
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
	}
	return nil
}

func (r *runner) ensureTenantKey(ctx context.Context) (string, error) {
	if r.cfg.TenantKey != "" {
		return r.cfg.TenantKey, nil
	}
	status, body, err := r.request(ctx, http.MethodPost, "/admin/tenants/"+url.PathEscape(r.cfg.TenantID)+"/keys", r.cfg.AdminKey, "", map[string]any{
		"label":   "ha-pressure",
		"purpose": "ha_pressure",
	})
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
	if resp.Key == "" {
		return "", fmt.Errorf("tenant key missing from response: %s", bodySummary(body))
	}
	return resp.Key, nil
}

func (r *runner) seedLeasePool(ctx context.Context, prefix string, total int, ttl time.Duration) ([]leaseInfo, error) {
	leases := make([]leaseInfo, 0, total)
	for i := 0; i < total; i++ {
		key := r.uniqueKey(prefix, 0, i)
		lease, err := r.createLeaseDTO(ctx, key, ttl)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

func (r *runner) createLease(ctx context.Context, idempotency string, ttl time.Duration) (int, error) {
	status, _, err := r.request(ctx, http.MethodPost, "/v1/leases", r.tenantKey, idempotency, createLeasePayload(idempotency, ttl))
	return status, err
}

func (r *runner) createLeaseDTO(ctx context.Context, idempotency string, ttl time.Duration) (leaseInfo, error) {
	status, body, err := r.request(ctx, http.MethodPost, "/v1/leases", r.tenantKey, idempotency, createLeasePayload(idempotency, ttl))
	if err != nil {
		return leaseInfo{}, err
	}
	if status != http.StatusCreated {
		return leaseInfo{}, fmt.Errorf("create lease status %d: %s", status, bodySummary(body))
	}
	var lease leaseInfo
	if err := json.Unmarshal(body, &lease); err != nil {
		return leaseInfo{}, err
	}
	if lease.LeaseID == "" || lease.Password == "" {
		return leaseInfo{}, fmt.Errorf("lease response incomplete: %s", bodySummary(body))
	}
	return lease, nil
}

func (r *runner) renewLease(ctx context.Context, lease leaseInfo) (int, error) {
	status, _, err := r.request(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(lease.LeaseID)+":renew", r.tenantKey, "", nil)
	return status, err
}

func (r *runner) validateLease(ctx context.Context, lease leaseInfo) (int, error) {
	endpoint, err := url.Parse(r.base + "/v1/gateway/validate")
	if err != nil {
		return 0, err
	}
	query := endpoint.Query()
	query.Set("tenant_id", r.cfg.TenantID)
	query.Set("lease_id", lease.LeaseID)
	query.Set("target", defaultTarget)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("ProxyHarbor-Key", r.cfg.AdminKey)
	req.Header.Set("ProxyHarbor-Password", lease.Password)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, readErr := io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, readErr
}

func (r *runner) waitReady(ctx context.Context) error {
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
	return fmt.Errorf("ready timeout: %s", last)
}

func (r *runner) request(ctx context.Context, method, path, key, idempotency string, payload any) (int, []byte, error) {
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
	raw, readErr := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, readErr
}

func createLeasePayload(subjectID string, ttl time.Duration) map[string]any {
	seconds := int64(math.Ceil(ttl.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return map[string]any{
		"subject":      map[string]any{"subject_type": "workload", "subject_id": subjectID},
		"resource_ref": map[string]any{"kind": "url", "id": defaultTarget},
		"ttl_seconds":  seconds,
	}
}

func (r *runner) uniqueKey(prefix string, worker, sequence int) string {
	return fmt.Sprintf("%s-%d-%d-%d", prefix, worker, sequence, r.ids.Add(1))
}

func newAccumulator() *accumulator {
	return &accumulator{
		statuses: map[int]int{},
		operations: map[string]*operationAccumulator{
			opValidate:    {Statuses: map[int]int{}},
			opLeaseCreate: {Statuses: map[int]int{}},
			opLeaseRenew:  {Statuses: map[int]int{}},
		},
	}
}

func (a *accumulator) Add(result operationResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total++
	if result.Success {
		a.success++
	} else {
		a.failure++
	}
	a.statuses[result.Status]++
	op, ok := a.operations[result.Operation]
	if !ok {
		op = &operationAccumulator{Statuses: map[int]int{}}
		a.operations[result.Operation] = op
	}
	op.Total++
	if result.Success {
		op.Success++
	} else {
		op.Failure++
	}
	op.Statuses[result.Status]++
	op.Latency = append(op.Latency, float64(result.Latency.Milliseconds()))
}

func (a *accumulator) Report(meta reportMeta) pressureReport {
	a.mu.Lock()
	defer a.mu.Unlock()

	if meta.FinishedAt.IsZero() {
		meta.FinishedAt = time.Now().UTC()
	}
	if meta.StartedAt.IsZero() {
		meta.StartedAt = meta.FinishedAt.Add(-meta.Elapsed)
	}

	report := pressureReport{
		Mode:               meta.Mode,
		BaseURL:            meta.BaseURL,
		Concurrency:        meta.Concurrency,
		SamplesPerOp:       meta.SamplesPerOp,
		Total:              a.total,
		Success:            a.success,
		Failure:            a.failure,
		OverallErrorRate:   ratio(a.failure, a.total),
		ElapsedMS:          float64(meta.Elapsed.Milliseconds()),
		StartedAt:          meta.StartedAt,
		FinishedAt:         meta.FinishedAt,
		StatusDistribution: cloneIntMap(a.statuses),
		Operations:         map[string]operationReport{},
	}
	for _, name := range orderedOperations {
		report.Operations[name] = buildOperationReport(name, a.operations[name])
	}
	report.SoakThreshold = evaluateSoakThreshold(meta, report)
	report.Passed = report.Failure == 0 && allOperationsPass(report.Operations) && (meta.Mode != "soak" || report.SoakThreshold.Pass)
	return report
}

func buildOperationReport(name string, op *operationAccumulator) operationReport {
	if op == nil {
		op = &operationAccumulator{Statuses: map[int]int{}}
	}
	latency := append([]float64(nil), op.Latency...)
	sort.Float64s(latency)
	report := operationReport{
		Total:              op.Total,
		Success:            op.Success,
		Failure:            op.Failure,
		ErrorRate:          ratio(op.Failure, op.Total),
		P50MS:              percentile(latency, 0.50),
		P95MS:              percentile(latency, 0.95),
		P99MS:              percentile(latency, 0.99),
		MaxMS:              percentile(latency, 1),
		StatusDistribution: cloneIntMap(op.Statuses),
	}
	report.Threshold = evaluateOperationThreshold(name, report)
	return report
}

func evaluateOperationThreshold(name string, report operationReport) operationThresholdCheck {
	targets := map[string]struct {
		p95 float64
		p99 float64
	}{
		opValidate:    {p95: 50, p99: 150},
		opLeaseCreate: {p95: 150, p99: 350},
		opLeaseRenew:  {p95: 120, p99: 300},
	}
	target := targets[name]
	check := operationThresholdCheck{
		TargetP95MS:  target.p95,
		TargetP99MS:  target.p99,
		MaxErrorRate: 0,
		Pass:         report.Total > 0,
	}
	if report.Total == 0 {
		check.Pass = false
		check.Violations = append(check.Violations, "no samples collected")
		return check
	}
	if report.P95MS >= target.p95 {
		check.Pass = false
		check.Violations = append(check.Violations, fmt.Sprintf("p95 %.0fms >= %.0fms", report.P95MS, target.p95))
	}
	if report.P99MS >= target.p99 {
		check.Pass = false
		check.Violations = append(check.Violations, fmt.Sprintf("p99 %.0fms >= %.0fms", report.P99MS, target.p99))
	}
	if report.ErrorRate > check.MaxErrorRate {
		check.Pass = false
		check.Violations = append(check.Violations, fmt.Sprintf("error rate %.4f > %.4f", report.ErrorRate, check.MaxErrorRate))
	}
	return check
}

func evaluateSoakThreshold(meta reportMeta, report pressureReport) soakThresholdResult {
	check := soakThresholdResult{
		RequiredConcurrency: 500,
		RequiredDurationMS:  float64((10 * time.Minute).Milliseconds()),
		MaxErrorRate:        0.005,
	}
	if meta.Mode != "soak" {
		check.Skipped = true
		return check
	}
	check.Pass = true
	if meta.Concurrency < check.RequiredConcurrency {
		check.Pass = false
		check.Violations = append(check.Violations, fmt.Sprintf("concurrency %d < %d", meta.Concurrency, check.RequiredConcurrency))
	}
	if meta.Elapsed < 10*time.Minute {
		check.Pass = false
		check.Violations = append(check.Violations, fmt.Sprintf("duration %s < 10m0s", meta.Elapsed.Round(time.Second)))
	}
	if report.Total < requiredSoakCountHint {
		check.Pass = false
		check.Violations = append(check.Violations, fmt.Sprintf("total requests %d < %d", report.Total, requiredSoakCountHint))
	}
	if report.OverallErrorRate >= check.MaxErrorRate {
		check.Pass = false
		check.Violations = append(check.Violations, fmt.Sprintf("error rate %.4f >= %.4f", report.OverallErrorRate, check.MaxErrorRate))
	}
	return check
}

func allOperationsPass(operations map[string]operationReport) bool {
	for _, name := range orderedOperations {
		if !operations[name].Threshold.Pass {
			return false
		}
	}
	return true
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

func writeJSONReport(w io.Writer, report pressureReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func cloneIntMap(src map[int]int) map[int]int {
	out := make(map[int]int, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func startDocker(ctx context.Context, cfg config) error {
	if !cfg.DockerSkipBuild {
		root := projectRoot()
		build := exec.CommandContext(ctx, "docker", "build", "--pull=false", "-t", "proxyharbor:ha-test", ".")
		build.Dir = root
		build.Stdout = os.Stderr
		build.Stderr = os.Stderr
		build.Env = scrubSecretEnv(os.Environ())
		if err := build.Run(); err != nil {
			return fmt.Errorf("docker build: %w", err)
		}
	}
	return compose(ctx, cfg, "up", "-d", "--wait", "--force-recreate", "--no-build")
}

func compose(ctx context.Context, cfg config, args ...string) error {
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
	fullArgs := addComposeEnvFile(append([]string{"compose", "-f", composeFile}, args...), envFile)
	cmd := exec.CommandContext(ctx, "docker", fullArgs...)
	cmd.Dir = root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = scrubSecretEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(fullArgs, " "), err)
	}
	return nil
}

func cleanupCompose(cfg config, timeout time.Duration, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = compose(ctx, cfg, args...)
}

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

func localHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               func(*http.Request) (*url.URL, error) { return nil, nil },
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 1024,
			IdleConnTimeout:     90 * time.Second,
		},
	}
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

func randomHex(bytesLen int) (string, error) {
	raw := make([]byte, bytesLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
