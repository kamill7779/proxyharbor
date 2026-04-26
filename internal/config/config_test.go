package config

import (
	"strings"
	"testing"
)

func TestParseTenantKeys_Valid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "C1 single entry",
			raw:  "tenanta:abcdefghijklmnop",
			want: map[string]string{"abcdefghijklmnop": "tenanta"},
		},
		{
			name: "C2 multiple entries",
			raw:  "tenanta:keykeykeykeykey1,tenantb:keykeykeykeykey2",
			want: map[string]string{"keykeykeykeykey1": "tenanta", "keykeykeykeykey2": "tenantb"},
		},
		{
			name: "trailing whitespace tolerated",
			raw:  "  tenanta:keykeykeykeykey1 , tenantb:keykeykeykeykey2 ",
			want: map[string]string{"keykeykeykeykey1": "tenanta", "keykeykeykeykey2": "tenantb"},
		},
		{
			name: "key may contain colon",
			raw:  "tenanta:abcd:efgh:ijkl:mnop",
			want: map[string]string{"abcd:efgh:ijkl:mnop": "tenanta"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTenantKeys(tc.raw, 16)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len=%d want=%d (got=%v)", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q -> %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestParseTenantKeys_Invalid(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		minKeyLen int
		errSubstr string
	}{
		{name: "C3 key too short", raw: "tenanta:short", minKeyLen: 16, errSubstr: "shorter than"},
		{name: "C4 invalid tenant id (uppercase)", raw: "Bad_Tenant:keykeykeykeykey1", minKeyLen: 16, errSubstr: "invalid tenant id"},
		{name: "C4b invalid tenant id (special char)", raw: "te.nant:keykeykeykeykey1", minKeyLen: 16, errSubstr: "invalid tenant id"},
		{name: "C5 duplicate key", raw: "a:keykeykeykeykey1,b:keykeykeykeykey1", minKeyLen: 16, errSubstr: "duplicate tenant key"},
		{name: "C6 duplicate tenant", raw: "a:keykeykeykeykey1,a:keykeykeykeykey2", minKeyLen: 16, errSubstr: "duplicate tenant"},
		{name: "missing colon", raw: "tenantAkey", minKeyLen: 16, errSubstr: "expected tenant:key"},
		{name: "empty key after colon", raw: "tenantA:", minKeyLen: 16, errSubstr: "expected tenant:key"},
		{name: "empty tenant before colon", raw: ":keykeykeykeykey1", minKeyLen: 16, errSubstr: "expected tenant:key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTenantKeys(tc.raw, tc.minKeyLen)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Fatalf("err=%q does not contain %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

func TestParseTenantKeys_EmptyReturnsNil(t *testing.T) {
	got, err := parseTenantKeys("", 16)
	if err != nil || got != nil {
		t.Fatalf("empty input: got=%v err=%v", got, err)
	}
	got, err = parseTenantKeys("   ", 16)
	if err != nil || got != nil {
		t.Fatalf("whitespace input: got=%v err=%v", got, err)
	}
}

func TestConfigValidate_AuthModes(t *testing.T) {
	base := func() Config {
		return Config{
			Role:                  "all",
			LogFormat:             "json",
			LogLevel:              "info",
			StorageDriver:         DriverMemory,
			Selector:              "zfair",
			SelectorRedisRequired: false,
			HealthBufferMax:       10,
			ZFairQuantum:          1,
			ZFairDefaultLatencyMS: 1,
			ZFairMaxPromote:       1,
			StickyPolicy:          "none",
		}
	}

	t.Run("C7 legacy only", func(t *testing.T) {
		c := base()
		c.AuthKey = "legacy"
		if err := c.validate(); err != nil {
			t.Fatalf("expected ok, got %v", err)
		}
	})

	t.Run("tenant-keys only", func(t *testing.T) {
		c := base()
		c.TenantKeys = map[string]string{"keykeykeykeykey1": "tenantA"}
		if err := c.validate(); err != nil {
			t.Fatalf("expected ok, got %v", err)
		}
	})

	t.Run("C8 both set is ambiguous", func(t *testing.T) {
		c := base()
		c.AuthKey = "legacy"
		c.TenantKeys = map[string]string{"keykeykeykeykey1": "tenantA"}
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("expected ambiguous error, got %v", err)
		}
	})

	t.Run("C9 neither set", func(t *testing.T) {
		c := base()
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), "auth key is required") {
			t.Fatalf("expected required error, got %v", err)
		}
	})
}
