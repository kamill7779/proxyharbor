package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteMetricsFormatsGaugeValue(t *testing.T) {
	registryMu.Lock()
	oldRegistry := registry
	registry = nil
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		registry = oldRegistry
		registryMu.Unlock()
	}()

	gauge := NewGauge("proxyharbor_test_gauge", "test gauge")
	gauge.Set(1.5)

	rec := httptest.NewRecorder()
	writeMetrics(rec)
	body := rec.Body.String()
	if !strings.Contains(body, "proxyharbor_test_gauge 1.5") {
		t.Fatalf("metrics body = %q, want gauge value 1.5", body)
	}
	if strings.Contains(body, "proxyharbor_test_gauge 1500") {
		t.Fatalf("metrics body exported scaled gauge: %q", body)
	}
}

func TestWriteMetricsPreservesHistogramSumPrecision(t *testing.T) {
	registryMu.Lock()
	oldRegistry := registry
	registry = nil
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		registry = oldRegistry
		registryMu.Unlock()
	}()

	histogram := NewHistogram("proxyharbor_test_histogram", "test histogram", []float64{1, 10})
	histogram.Observe(0.5)
	histogram.Observe(1.25)

	rec := httptest.NewRecorder()
	writeMetrics(rec)
	body := rec.Body.String()
	if !strings.Contains(body, "\nproxyharbor_test_histogram_sum 1.75\n") {
		t.Fatalf("metrics body = %q, want histogram sum 1.75", body)
	}
	if strings.Contains(body, `\nproxyharbor_test_histogram_count`) {
		t.Fatalf("metrics body contains literal backslash-n before count: %q", body)
	}
}

func TestSelectorResultMetricsUseLowCardinalityLabels(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMetrics(rec)
	body := rec.Body.String()
	if !strings.Contains(body, `proxyharbor_selector_results_total{mode="zfair",result="redis_error",error_kind="selector_redis"}`) {
		t.Fatalf("metrics body missing zfair redis_error selector result: %q", body)
	}
	if !strings.Contains(body, `proxyharbor_selector_results_total{mode="zfair",result="selected",error_kind="none"}`) {
		t.Fatalf("metrics body missing zfair selected selector result: %q", body)
	}
	if strings.Contains(body, "tenant_id=") || strings.Contains(body, "proxy_id=") {
		t.Fatalf("selector metrics contain high-cardinality labels: %q", body)
	}
}

func TestCacheInvalidationMetricsUseLowCardinalityLabels(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMetrics(rec)
	body := rec.Body.String()
	for _, want := range []string{
		`proxyharbor_cache_invalidation_total{cache="auth",action="refresh",transport="redis",result="published",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="catalog",action="invalidate",transport="redis",result="applied",error_kind="none"}`,
		`proxyharbor_cache_invalidation_total{cache="lease",action="invalidate",transport="redis",result="error",error_kind="redis"}`,
		`proxyharbor_cache_invalidation_total{cache="auth",action="refresh",transport="polling",result="applied",error_kind="none"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %s: %q", want, body)
		}
	}
	for _, forbidden := range []string{"tenant_id=", "key_id=", "proxy_id=", "lease_id=", "request_id="} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("cache invalidation metrics contain high-cardinality label %s: %q", forbidden, body)
		}
	}
}

func TestRuntimeMetricsUseLowCardinalityLabels(t *testing.T) {
	RecordRuntimeStartupResult("started", "none")
	RecordRuntimeStartupResult("error", "config")
	RecordRuntimeShutdownResult("graceful", "none")
	RecordRuntimeShutdownResult("started", "none")
	RecordRuntimeConfigValidationResult("error", "missing_secret")
	RecordRuntimeDependencyStatus("mysql", "degraded", "mysql")
	RecordRuntimeDependencyStatus("sqlite", "ready", "none")
	RecordRuntimeDependencyStatus("memory", "degraded", "backend")

	rec := httptest.NewRecorder()
	writeMetrics(rec)
	body := rec.Body.String()
	for _, want := range []string{
		`proxyharbor_runtime_startup_total{component="server",result="started",error_kind="none"}`,
		`proxyharbor_runtime_startup_total{component="server",result="error",error_kind="config"}`,
		`proxyharbor_runtime_shutdown_total{component="server",result="graceful",error_kind="none"}`,
		`proxyharbor_runtime_shutdown_total{component="server",result="started",error_kind="none"}`,
		`proxyharbor_runtime_config_validation_total{component="config",result="error",error_kind="missing_secret"}`,
		`proxyharbor_runtime_dependency_status{dependency="mysql",result="degraded",error_kind="mysql"} 1`,
		`proxyharbor_runtime_dependency_status{dependency="sqlite",result="ready",error_kind="none"} 1`,
		`proxyharbor_runtime_dependency_status{dependency="memory",result="degraded",error_kind="backend"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %s: %q", want, body)
		}
	}
	for _, forbidden := range []string{"tenant_id=", "key_id=", "proxy_id=", "lease_id=", "request_id=", "secret=", "password="} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("runtime metrics contain forbidden label/content %s: %q", forbidden, body)
		}
	}
}

func TestAuthCacheStaleSecondsMetricExists(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMetrics(rec)
	body := rec.Body.String()
	if !strings.Contains(body, "proxyharbor_auth_cache_stale_seconds") {
		t.Fatalf("metrics body missing auth stale seconds gauge: %q", body)
	}
}
