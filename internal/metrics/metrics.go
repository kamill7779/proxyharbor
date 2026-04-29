package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type metricType int

const (
	metricCounter   metricType = iota
	metricGauge     metricType = iota
	metricHistogram metricType = iota
)

type labeledMetric struct {
	typ     metricType
	name    string
	help    string
	labels  string
	mu      sync.Mutex
	val     *atomic.Int64
	gval    *atomic.Int64
	bounds  []float64
	buckets []int64
	sum     float64
	count   int64
}

var (
	registryMu sync.Mutex
	registry   []*labeledMetric
)

func register(m *labeledMetric) {
	registryMu.Lock()
	registry = append(registry, m)
	registryMu.Unlock()
}

func snapshot() []*labeledMetric {
	registryMu.Lock()
	defer registryMu.Unlock()
	cp := make([]*labeledMetric, len(registry))
	copy(cp, registry)
	return cp
}

// Counter represents a monotonically increasing counter.
type Counter struct{ m *labeledMetric }

func NewCounter(name, help string) *Counter {
	m := &labeledMetric{typ: metricCounter, name: name, help: help, val: new(atomic.Int64)}
	register(m)
	return &Counter{m: m}
}

func NewCounterWithLabels(name, help, labels string) *Counter {
	m := &labeledMetric{typ: metricCounter, name: name, help: help, labels: labels, val: new(atomic.Int64)}
	register(m)
	return &Counter{m: m}
}

func (c *Counter) Inc() { c.m.val.Add(1) }

func (c *Counter) Add(n int64) {
	if n <= 0 {
		return
	}
	c.m.val.Add(n)
}

// Gauge represents a value that can go up and down.
type Gauge struct{ m *labeledMetric }

func NewGauge(name, help string) *Gauge {
	m := &labeledMetric{typ: metricGauge, name: name, help: help, gval: new(atomic.Int64)}
	register(m)
	return &Gauge{m: m}
}

func (g *Gauge) Set(v float64) { g.m.gval.Store(int64(v * 1000)) }

func (g *Gauge) Value() float64 { return float64(g.m.gval.Load()) / 1000 }

// Histogram records observations into pre-defined buckets.
type Histogram struct{ m *labeledMetric }

func NewHistogram(name, help string, bounds []float64) *Histogram {
	m := &labeledMetric{
		typ:     metricHistogram,
		name:    name,
		help:    help,
		bounds:  bounds,
		buckets: make([]int64, len(bounds)),
	}
	register(m)
	return &Histogram{m: m}
}

func NewHistogramWithLabels(name, help, labels string, bounds []float64) *Histogram {
	m := &labeledMetric{
		typ:     metricHistogram,
		name:    name,
		help:    help,
		labels:  labels,
		bounds:  bounds,
		buckets: make([]int64, len(bounds)),
	}
	register(m)
	return &Histogram{m: m}
}

func (h *Histogram) Observe(v float64) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	h.m.count++
	h.m.sum += v
	for i, bound := range h.m.bounds {
		if v <= bound {
			h.m.buckets[i]++
			return
		}
	}
}

// --- v0.4.0 metrics ---

var (
	LeaseCreateTotal = NewCounter("proxyharbor_lease_create_total", "Total number of lease creations")
	LeaseCreateFail  = NewCounter("proxyharbor_lease_create_fail_total", "Failed lease creations")

	LeaseRenewTotal = NewCounter("proxyharbor_lease_renew_total", "Total number of lease renewals")
	LeaseRenewFail  = NewCounter("proxyharbor_lease_renew_fail_total", "Failed lease renewals")

	LeaseRevokeTotal = NewCounter("proxyharbor_lease_revoke_total", "Total number of lease revocations")
	LeaseRevokeFail  = NewCounter("proxyharbor_lease_revoke_fail_total", "Failed lease revocations")

	LeaseValidateTotal = NewCounter("proxyharbor_lease_validate_total", "Total number of lease validations")
	LeaseValidateFail  = NewCounter("proxyharbor_lease_validate_fail_total", "Failed lease validations")

	SelectorErrors    = NewCounter("proxyharbor_selector_errors_total", "Total selector errors")
	NoHealthyProxy    = NewCounter("proxyharbor_no_healthy_proxy_total", "Total no-healthy-proxy responses")
	SelectorLatencyMS = NewHistogram("proxyharbor_selector_latency_ms", "Selector latency in milliseconds", []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000})

	SelectorLocalSelected = NewCounterWithLabels("proxyharbor_selector_selected_total", "Total successful selector selections by low-cardinality mode", "mode=\"local\"")
	SelectorZFairSelected = NewCounterWithLabels("proxyharbor_selector_selected_total", "Total successful selector selections by low-cardinality mode", "mode=\"zfair\"")
	SelectorLocalErrors   = NewCounterWithLabels("proxyharbor_selector_errors_by_mode_total", "Total selector errors by low-cardinality mode and result", "mode=\"local\",result=\"error\"")
	SelectorZFairErrors   = NewCounterWithLabels("proxyharbor_selector_errors_by_mode_total", "Total selector errors by low-cardinality mode and result", "mode=\"zfair\",result=\"error\"")
	SelectorLocalLatency  = NewHistogramWithLabels("proxyharbor_selector_latency_by_mode_ms", "Selector latency in milliseconds by low-cardinality mode", "mode=\"local\"", []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000})
	SelectorZFairLatency  = NewHistogramWithLabels("proxyharbor_selector_latency_by_mode_ms", "Selector latency in milliseconds by low-cardinality mode", "mode=\"zfair\"", []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000})

	AuthRefreshSuccess = NewCounter("proxyharbor_auth_refresh_success_total", "Successful auth cache refreshes")
	AuthRefreshFail    = NewCounter("proxyharbor_auth_refresh_fail_total", "Failed auth cache refreshes")

	HealthEventsTotal  = NewCounter("proxyharbor_health_events_total", "Total health feedback events recorded")
	AuditWriteFailures = NewCounter("proxyharbor_audit_write_failures_total", "Failed audit writes")

	GatewayValidateTotal = NewCounter("proxyharbor_gateway_validate_total", "Total gateway validation requests")
	GatewayValidateFail  = NewCounter("proxyharbor_gateway_validate_fail_total", "Failed gateway validation requests")
)

// Handler returns an http.Handler that serves Prometheus text-format metrics.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w)
	})
}

func writeMetrics(w http.ResponseWriter) {
	metrics := snapshot()
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].name < metrics[j].name })

	processed := map[*labeledMetric]bool{}
	headerWritten := map[string]bool{}
	for _, m := range metrics {
		if processed[m] {
			continue
		}
		processed[m] = true
		switch m.typ {
		case metricCounter:
			writeMetricHeader(w, headerWritten, m, "counter")
			fmt.Fprintf(w, "%s%s %d\n\n", m.name, formatLabels(m.labels), m.val.Load())
		case metricGauge:
			writeMetricHeader(w, headerWritten, m, "gauge")
			fmt.Fprintf(w, "%s %g\n\n", m.name, float64(m.gval.Load())/1000)
		case metricHistogram:
			writeMetricHeader(w, headerWritten, m, "histogram")
			m.mu.Lock()
			buckets := append([]int64(nil), m.buckets...)
			sum := m.sum
			count := m.count
			m.mu.Unlock()
			last := int64(0)
			for i := range m.bounds {
				v := buckets[i]
				le := fmt.Sprintf("%.0f", m.bounds[i])
				fmt.Fprintf(w, "%s_bucket%s %d\n", m.name, mergeLabels(m.labels, "le=\""+le+"\""), v+last)
				last += v
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", m.name, mergeLabels(m.labels, "le=\"+Inf\""), count)
			fmt.Fprintf(w, "%s_sum%s %g\\n", m.name, formatLabels(m.labels), sum)
			fmt.Fprintf(w, "%s_count%s %d\n\n", m.name, formatLabels(m.labels), count)
		}
	}

	if since := time.Since(processStart).Milliseconds(); since > 0 {
		fmt.Fprintf(w, "# HELP proxyharbor_process_uptime_seconds Process uptime in seconds\n")
		fmt.Fprintf(w, "# TYPE proxyharbor_process_uptime_seconds gauge\n")
		fmt.Fprintf(w, "proxyharbor_process_uptime_seconds %.3f\n\n", float64(since)/1000.0)
	}
	fmt.Fprintf(w, "# HELP proxyharbor_instance_up Whether this ProxyHarbor instance is serving metrics.\n")
	fmt.Fprintf(w, "# TYPE proxyharbor_instance_up gauge\n")
	fmt.Fprintf(w, "proxyharbor_instance_up 1\n")
}

var processStart = time.Now()

func formatLabels(labels string) string {
	if labels == "" {
		return ""
	}
	return "{" + labels + "}"
}

func mergeLabels(labels, extra string) string {
	if labels == "" {
		return "{" + extra + "}"
	}
	return "{" + labels + "," + extra + "}"
}

func writeMetricHeader(w http.ResponseWriter, written map[string]bool, m *labeledMetric, typ string) {
	if written[m.name] {
		return
	}
	written[m.name] = true
	fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help)
	fmt.Fprintf(w, "# TYPE %s %s\n", m.name, typ)
}
