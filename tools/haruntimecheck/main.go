package main

import (
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
	"strings"
	"time"
)

type config struct {
	BaseURL         string
	AdminKey        string
	KeyPepper       string
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

func main() {
	cfg := parseFlags()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "haruntimecheck:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.BaseURL, "base-url", envDefault("PROXYHARBOR_BASE_URL", "http://127.0.0.1:18081"), "ProxyHarbor HA load-balancer URL")
	flag.StringVar(&cfg.AdminKey, "admin-key", "", "admin key; defaults to PROXYHARBOR_ADMIN_KEY")
	flag.StringVar(&cfg.KeyPepper, "key-pepper", "", "key pepper used when -docker starts compose; defaults to PROXYHARBOR_KEY_PEPPER")
	flag.BoolVar(&cfg.Docker, "docker", false, "start docker-compose HA test topology")
	flag.BoolVar(&cfg.DockerSkipBuild, "docker-skip-build", false, "reuse existing proxyharbor:ha-test image when starting docker HA topology")
	flag.StringVar(&cfg.ComposeFile, "compose-file", "docker-compose.ha-test.yaml", "compose file used with -docker")
	flag.DurationVar(&cfg.Timeout, "timeout", 5*time.Minute, "overall timeout")
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
	var err error
	if cfg.AdminKey == "" {
		cfg.AdminKey, err = randomHex(32)
		if err != nil {
			return err
		}
	}
	if cfg.KeyPepper == "" {
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
	r := runner{
		cfg:       cfg,
		client:    localHTTPClient(10 * time.Second),
		base:      strings.TrimRight(cfg.BaseURL, "/"),
		instances: cfg.InstanceURLs,
	}
	checks := []struct {
		name string
		fn   func(context.Context) error
	}{
		{name: "initial readiness", fn: r.checkInitialReadiness},
		{name: "single instance sigterm recovery", fn: r.checkSingleInstanceStopRecovery},
		{name: "redis readiness recovery", fn: r.checkRedisRecovery},
		{name: "mysql readiness recovery", fn: r.checkMySQLRecovery},
		{name: "runtime metrics", fn: r.checkRuntimeMetrics},
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

func (r runner) checkInitialReadiness(ctx context.Context) error {
	if err := r.waitAllReady(ctx, 90*time.Second); err != nil {
		return err
	}
	return r.forEachInstance(ctx, 5*time.Second, func(base string) error {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/healthz", "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("healthz status %d: %s", status, bodySummary(body))
		}
		return nil
	})
}

func (r runner) checkSingleInstanceStopRecovery(ctx context.Context) error {
	if !r.cfg.Docker {
		return nil
	}
	if err := compose(ctx, r.cfg, "stop", "-t", "25", "proxyharbor-a"); err != nil {
		return fmt.Errorf("stop proxyharbor-a: %w", err)
	}
	restored := false
	defer func() {
		if !restored {
			cleanupCompose(r.cfg, 60*time.Second, "up", "-d", "--wait", "proxyharbor-a")
		}
	}()
	if err := r.waitLBReady(ctx, 20*time.Second); err != nil {
		return err
	}
	if err := compose(ctx, r.cfg, "up", "-d", "--wait", "proxyharbor-a"); err != nil {
		return fmt.Errorf("start proxyharbor-a: %w", err)
	}
	if err := r.waitAllReady(ctx, 45*time.Second); err != nil {
		return err
	}
	restored = true
	return nil
}

func (r runner) checkRedisRecovery(ctx context.Context) error {
	if !r.cfg.Docker {
		return nil
	}
	if err := compose(ctx, r.cfg, "stop", "redis"); err != nil {
		return fmt.Errorf("stop redis: %w", err)
	}
	restored := false
	defer func() {
		if !restored {
			cleanupCompose(r.cfg, 60*time.Second, "up", "-d", "--wait", "redis")
		}
	}()
	if err := r.waitAllDegraded(ctx, 35*time.Second, "redis"); err != nil {
		return err
	}
	if err := r.assertAllHealthzOK(ctx); err != nil {
		return err
	}
	if err := compose(ctx, r.cfg, "up", "-d", "--wait", "redis"); err != nil {
		return fmt.Errorf("start redis: %w", err)
	}
	if err := r.waitAllReadySubscribed(ctx, 45*time.Second); err != nil {
		return err
	}
	restored = true
	return nil
}

func (r runner) checkMySQLRecovery(ctx context.Context) error {
	if !r.cfg.Docker {
		return nil
	}
	if err := compose(ctx, r.cfg, "stop", "mysql"); err != nil {
		return fmt.Errorf("stop mysql: %w", err)
	}
	restored := false
	defer func() {
		if !restored {
			cleanupCompose(r.cfg, 90*time.Second, "up", "-d", "--wait", "mysql")
		}
	}()
	if err := r.waitAllDegraded(ctx, 35*time.Second, "mysql"); err != nil {
		return err
	}
	if err := r.assertAllHealthzOK(ctx); err != nil {
		return err
	}
	if err := compose(ctx, r.cfg, "up", "-d", "--wait", "mysql"); err != nil {
		return fmt.Errorf("start mysql: %w", err)
	}
	if err := r.waitAllReadySubscribed(ctx, 60*time.Second); err != nil {
		return err
	}
	restored = true
	return nil
}

func (r runner) checkRuntimeMetrics(ctx context.Context) error {
	if err := r.waitAllReadySubscribed(ctx, 10*time.Second); err != nil {
		return err
	}
	return r.forEachInstance(ctx, 5*time.Second, func(base string) error {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/metrics", r.cfg.AdminKey)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("metrics status %d: %s", status, bodySummary(body))
		}
		text := string(body)
		for _, want := range []string{
			`proxyharbor_runtime_startup_total{component="server",result="started",error_kind="none"}`,
			`proxyharbor_runtime_config_validation_total{component="config",result="ok",error_kind="none"}`,
			`proxyharbor_runtime_dependency_status{dependency="mysql",result="ready",error_kind="none"}`,
			`proxyharbor_runtime_dependency_status{dependency="redis_cache",result="ready",error_kind="none"}`,
			`proxyharbor_runtime_dependency_status{dependency="redis_selector",result="ready",error_kind="none"}`,
			`proxyharbor_runtime_dependency_status{dependency="cache_invalidation",result="ready",error_kind="none"}`,
		} {
			value, ok := metricValue(text, want)
			if !ok || value <= 0 {
				return fmt.Errorf("metrics series %s value=%g ok=%v, want > 0", want, value, ok)
			}
		}
		for _, forbidden := range []string{"tenant_id=", "key_id=", "proxy_id=", "lease_id=", "request_id=", "password=", "secret="} {
			if strings.Contains(text, forbidden) {
				return fmt.Errorf("metrics contain forbidden label/content %s", forbidden)
			}
		}
		return nil
	})
}

func (r runner) waitAllReady(ctx context.Context, timeout time.Duration) error {
	return r.forEachInstance(ctx, timeout, func(base string) error {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/readyz", "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("readyz status %d: %s", status, bodySummary(body))
		}
		return nil
	})
}

func (r runner) waitAllReadySubscribed(ctx context.Context, timeout time.Duration) error {
	return r.forEachInstance(ctx, timeout, func(base string) error {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/readyz", "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("readyz status %d: %s", status, bodySummary(body))
		}
		if err := requireReadySubscribed(body); err != nil {
			return err
		}
		return nil
	})
}

func (r runner) waitAllDegraded(ctx context.Context, timeout time.Duration, wantKind string) error {
	return r.forEachInstance(ctx, timeout, func(base string) error {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/readyz", "")
		if err != nil {
			return err
		}
		if status != http.StatusServiceUnavailable {
			return fmt.Errorf("readyz status %d, want degraded: %s", status, bodySummary(body))
		}
		if err := requireErrorKind(body, wantKind); err != nil {
			return err
		}
		return nil
	})
}

func requireReadySubscribed(body string) error {
	var doc struct {
		CacheInvalidation struct {
			State string `json:"state"`
		} `json:"cache_invalidation"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return fmt.Errorf("decode readyz: %w", err)
	}
	if doc.CacheInvalidation.State != "subscribed" {
		return fmt.Errorf("readyz invalidation state %q, want subscribed: %s", doc.CacheInvalidation.State, bodySummary(body))
	}
	return nil
}

func requireErrorKind(body, wantKind string) error {
	var doc struct {
		ErrorKinds map[string]string `json:"error_kinds"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return fmt.Errorf("decode readyz: %w", err)
	}
	for dep, kind := range doc.ErrorKinds {
		if kind == wantKind || (kind == "timeout" && strings.Contains(dep, wantKind)) {
			return nil
		}
	}
	return fmt.Errorf("readyz missing error kind %q: %s", wantKind, bodySummary(body))
}

func (r runner) waitLBReady(ctx context.Context, timeout time.Duration) error {
	return retryUntil(ctx, timeout, func() error {
		status, body, err := r.requestAt(ctx, r.base, http.MethodGet, "/readyz", "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("lb readyz status %d: %s", status, bodySummary(body))
		}
		return nil
	})
}

func (r runner) assertAllHealthzOK(ctx context.Context) error {
	return r.forEachInstance(ctx, 5*time.Second, func(base string) error {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/healthz", "")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("healthz status %d: %s", status, bodySummary(body))
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

func (r runner) aggregateMetrics(ctx context.Context) (string, error) {
	bases := r.instances
	if len(bases) == 0 {
		bases = []string{r.base}
	}
	var out strings.Builder
	for _, base := range bases {
		status, body, err := r.requestAt(ctx, base, http.MethodGet, "/metrics", r.cfg.AdminKey)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("metrics status %d from %s: %s", status, base, bodySummary(body))
		}
		out.WriteString(body)
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
			continue
		}
		var value float64
		if _, err := fmt.Sscanf(fields[1], "%f", &value); err != nil {
			continue
		}
		total += value
		found = true
	}
	return total, found
}

func (r runner) requestAt(ctx context.Context, base, method, path, adminKey string) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return 0, "", err
	}
	if adminKey != "" {
		req.Header.Set("ProxyHarbor-Key", adminKey)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
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
		case <-time.After(250 * time.Millisecond):
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
	args := []string{"compose", "-f", cfg.ComposeFile, "up", "-d", "--wait", "--force-recreate"}
	if cfg.DockerSkipBuild {
		args = append(args, "--no-build")
	}
	return composeArgs(ctx, cfg, args...)
}

func compose(ctx context.Context, cfg config, args ...string) error {
	fullArgs := append([]string{"compose", "-f", cfg.ComposeFile}, args...)
	return composeArgs(ctx, cfg, fullArgs...)
}

func cleanupCompose(cfg config, timeout time.Duration, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = compose(ctx, cfg, args...)
}

func composeArgs(ctx context.Context, cfg config, args ...string) error {
	envFile, cleanup, err := composeEnvFile(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	args = addComposeEnvFile(args, envFile)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = scrubSecretEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func localHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
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

func bodySummary(body string) string {
	var doc struct {
		Error  string `json:"error"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err == nil {
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
