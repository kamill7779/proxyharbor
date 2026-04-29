package main

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSummaryComputesLatencyAndDistributions(t *testing.T) {
	results := []operationResult{
		{Operation: "lease_create", Status: 201, ProxyID: "proxy-a", Latency: 10 * time.Millisecond, Success: true},
		{Operation: "lease_create", Status: 201, ProxyID: "proxy-b", Latency: 20 * time.Millisecond, Success: true},
		{Operation: "renew", Status: 200, ProxyID: "proxy-a", Latency: 30 * time.Millisecond, Success: true},
		{Operation: "validate", Status: 500, Latency: 100 * time.Millisecond, Success: false},
	}

	summary := summarize("mixed", results, 200*time.Millisecond)

	if summary.Total != 4 || summary.Success != 3 || summary.Failure != 1 {
		t.Fatalf("summary counts = total:%d success:%d failure:%d", summary.Total, summary.Success, summary.Failure)
	}
	if summary.RPS != 20 {
		t.Fatalf("rps = %v, want 20", summary.RPS)
	}
	if summary.P50MS != 20 || summary.P90MS != 100 || summary.P95MS != 100 || summary.P99MS != 100 || summary.MaxMS != 100 {
		t.Fatalf("latencies = p50:%v p90:%v p95:%v p99:%v max:%v", summary.P50MS, summary.P90MS, summary.P95MS, summary.P99MS, summary.MaxMS)
	}
	if summary.StatusDistribution[201] != 2 || summary.StatusDistribution[200] != 1 || summary.StatusDistribution[500] != 1 {
		t.Fatalf("status distribution = %#v", summary.StatusDistribution)
	}
	if summary.ProxyDistribution["proxy-a"] != 2 || summary.ProxyDistribution["proxy-b"] != 1 {
		t.Fatalf("proxy distribution = %#v", summary.ProxyDistribution)
	}
}

func TestSummaryEncodesJSONAndCSV(t *testing.T) {
	summary := benchmarkSummary{
		Operation:          "lease_create",
		Total:              1,
		Success:            1,
		ElapsedMS:          50,
		RPS:                20,
		P50MS:              7,
		P90MS:              7,
		P95MS:              7,
		P99MS:              7,
		MaxMS:              7,
		StatusDistribution: map[int]int{201: 1},
		ProxyDistribution:  map[string]int{"proxy-a": 1},
		StartedAt:          time.Date(2026, 4, 29, 1, 2, 3, 0, time.UTC),
		FinishedAt:         time.Date(2026, 4, 29, 1, 2, 3, 50_000_000, time.UTC),
	}

	var jsonOut strings.Builder
	if err := writeJSONSummary(&jsonOut, summary); err != nil {
		t.Fatalf("writeJSONSummary() error = %v", err)
	}
	var decoded benchmarkSummary
	if err := json.Unmarshal([]byte(jsonOut.String()), &decoded); err != nil {
		t.Fatalf("json output is invalid: %v\n%s", err, jsonOut.String())
	}
	if decoded.StatusDistribution[201] != 1 || decoded.ProxyDistribution["proxy-a"] != 1 {
		t.Fatalf("decoded summary = %#v", decoded)
	}

	var csvOut strings.Builder
	if err := writeCSVSummary(&csvOut, summary); err != nil {
		t.Fatalf("writeCSVSummary() error = %v", err)
	}
	records, err := csv.NewReader(strings.NewReader(csvOut.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv output is invalid: %v\n%s", err, csvOut.String())
	}
	if len(records) != 2 {
		t.Fatalf("records len = %d, want 2", len(records))
	}
	if records[0][0] != "operation" || records[1][0] != "lease_create" {
		t.Fatalf("csv records = %#v", records)
	}
	if !strings.Contains(records[1][13], "201=1") || !strings.Contains(records[1][14], "proxy-a=1") {
		t.Fatalf("csv distributions = status:%q proxy:%q", records[1][13], records[1][14])
	}
}
