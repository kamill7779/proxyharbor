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
