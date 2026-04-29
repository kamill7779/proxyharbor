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
