package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSummarizeOperationsComputesPercentilesAndErrorRate(t *testing.T) {
	acc := newAccumulator()
	for _, result := range []operationResult{
		{Operation: opValidate, Status: 200, Latency: 10 * time.Millisecond, Success: true},
		{Operation: opValidate, Status: 200, Latency: 20 * time.Millisecond, Success: true},
		{Operation: opValidate, Status: 503, Latency: 40 * time.Millisecond, Success: false},
		{Operation: opLeaseCreate, Status: 201, Latency: 50 * time.Millisecond, Success: true},
		{Operation: opLeaseCreate, Status: 201, Latency: 80 * time.Millisecond, Success: true},
		{Operation: opLeaseRenew, Status: 200, Latency: 30 * time.Millisecond, Success: true},
	} {
		acc.Add(result)
	}

	report := acc.Report(reportMeta{
		Mode:         "pressure",
		Concurrency:  32,
		SamplesPerOp: 2,
		Elapsed:      250 * time.Millisecond,
	})

	if report.Total != 6 || report.Success != 5 || report.Failure != 1 {
		t.Fatalf("report counts = total:%d success:%d failure:%d", report.Total, report.Success, report.Failure)
	}
	if report.OverallErrorRate != 1.0/6.0 {
		t.Fatalf("overall error rate = %v, want %v", report.OverallErrorRate, 1.0/6.0)
	}
	validate := report.Operations[opValidate]
	if validate.P95MS != 40 || validate.P99MS != 40 || validate.ErrorRate != 1.0/3.0 {
		t.Fatalf("validate stats = %#v", validate)
	}
	if validate.StatusDistribution[200] != 2 || validate.StatusDistribution[503] != 1 {
		t.Fatalf("validate status distribution = %#v", validate.StatusDistribution)
	}
}

func TestReportEvaluatesV054Thresholds(t *testing.T) {
	acc := newAccumulator()
	for _, result := range []operationResult{
		{Operation: opValidate, Status: 200, Latency: 60 * time.Millisecond, Success: true},
		{Operation: opValidate, Status: 200, Latency: 170 * time.Millisecond, Success: true},
		{Operation: opLeaseCreate, Status: 201, Latency: 120 * time.Millisecond, Success: true},
		{Operation: opLeaseCreate, Status: 201, Latency: 360 * time.Millisecond, Success: true},
		{Operation: opLeaseRenew, Status: 200, Latency: 110 * time.Millisecond, Success: true},
		{Operation: opLeaseRenew, Status: 200, Latency: 310 * time.Millisecond, Success: true},
	} {
		acc.Add(result)
	}

	report := acc.Report(reportMeta{
		Mode:        "soak",
		Concurrency: 500,
		Elapsed:     10 * time.Minute,
	})

	if report.Passed {
		t.Fatalf("report passed = true, want false")
	}
	if report.Operations[opValidate].Threshold.Pass {
		t.Fatalf("validate threshold pass = true, want false")
	}
	if report.Operations[opLeaseCreate].Threshold.Pass {
		t.Fatalf("lease create threshold pass = true, want false")
	}
	if report.Operations[opLeaseRenew].Threshold.Pass {
		t.Fatalf("lease renew threshold pass = true, want false")
	}
	if report.SoakThreshold.Pass {
		t.Fatalf("soak threshold pass = true, want false")
	}
	if len(report.SoakThreshold.Violations) == 0 {
		t.Fatalf("expected soak threshold violations")
	}
}

func TestWriteJSONReportProducesMachineReadableOutput(t *testing.T) {
	acc := newAccumulator()
	acc.Add(operationResult{Operation: opValidate, Status: 200, Latency: 12 * time.Millisecond, Success: true})
	report := acc.Report(reportMeta{
		Mode:        "pressure",
		Concurrency: 8,
		Elapsed:     12 * time.Millisecond,
	})

	var out strings.Builder
	if err := writeJSONReport(&out, report); err != nil {
		t.Fatalf("writeJSONReport() error = %v", err)
	}

	var decoded pressureReport
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("invalid json output: %v\n%s", err, out.String())
	}
	if decoded.Mode != "pressure" || decoded.Operations[opValidate].StatusDistribution[200] != 1 {
		t.Fatalf("decoded report = %#v", decoded)
	}
}
