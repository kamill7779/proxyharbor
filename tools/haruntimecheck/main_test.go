package main

import "testing"

func TestReadyzParsingUsesJSONSemantics(t *testing.T) {
	readyBody := `{
		"role": "all",
		"status": "ready",
		"cache_invalidation": {
			"state": "subscribed"
		}
	}`
	if err := requireReadySubscribed(readyBody); err != nil {
		t.Fatalf("requireReadySubscribed() error = %v", err)
	}

	degradedBody := `{
		"role": "all",
		"status": "degraded",
		"error_kinds": {
			"redis_cache": "redis"
		}
	}`
	if err := requireErrorKind(degradedBody, "redis"); err != nil {
		t.Fatalf("requireErrorKind(redis) error = %v", err)
	}
	if err := requireErrorKind(degradedBody, "mysql"); err == nil {
		t.Fatal("requireErrorKind(mysql) error = nil, want missing kind error")
	}

	timeoutBody := `{
		"status": "degraded",
		"error_kinds": {
			"mysql": "timeout"
		}
	}`
	if err := requireErrorKind(timeoutBody, "mysql"); err != nil {
		t.Fatalf("requireErrorKind(mysql timeout) error = %v", err)
	}
}
