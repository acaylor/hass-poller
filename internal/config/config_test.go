package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStringWithDefault(t *testing.T) {
	tests := []struct {
		name, val, fallback, want string
	}{
		{"empty falls back", "", "default", "default"},
		{"whitespace falls back", "   ", "default", "default"},
		{"value used", "actual", "default", "actual"},
		{"value trimmed", "  actual  ", "default", "actual"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringWithDefault(tt.val, tt.fallback); got != tt.want {
				t.Errorf("stringWithDefault(%q, %q) = %q, want %q", tt.val, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestSplitCSVWithDefault(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		fallback []string
		want     []string
	}{
		{"empty with non-nil fallback returns copy of fallback", "", []string{"sensor.*"}, []string{"sensor.*"}},
		{"empty with nil fallback returns nil", "", nil, nil},
		{"whitespace-only with fallback", "   ", []string{"a"}, []string{"a"}},
		{"single value", "sensor.foo", nil, []string{"sensor.foo"}},
		{"multiple values", "sensor.foo,sensor.bar", nil, []string{"sensor.foo", "sensor.bar"}},
		{"trims whitespace around values", " a , b ,c ", nil, []string{"a", "b", "c"}},
		{"drops empty parts from doubled commas", "a,,b", nil, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCSVWithDefault(tt.raw, tt.fallback)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitCSVWithDefault(%q, %v) = %v, want %v", tt.raw, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestSplitCSVWithDefault_FallbackIsCopied(t *testing.T) {
	fallback := []string{"sensor.*"}
	out := splitCSVWithDefault("", fallback)
	out[0] = "mutated"
	if fallback[0] != "sensor.*" {
		t.Fatalf("fallback was mutated through returned slice: %v", fallback)
	}
}

func TestParseDurationWithDefault(t *testing.T) {
	tests := []struct {
		name, env string
		fallback  time.Duration
		want      time.Duration
	}{
		{"unset uses fallback", "", time.Minute, time.Minute},
		{"valid duration", "30s", time.Minute, 30 * time.Second},
		{"invalid duration falls back", "not-a-duration", time.Minute, time.Minute},
		{"whitespace only falls back", "   ", time.Minute, time.Minute},
		{"value with surrounding whitespace", "  10s  ", time.Minute, 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_DUR", tt.env)
			if got := parseDurationWithDefault("TEST_DUR", tt.fallback); got != tt.want {
				t.Errorf("parseDurationWithDefault(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestParseFloatWithDefault(t *testing.T) {
	tests := []struct {
		name, env string
		fallback  float64
		want      float64
	}{
		{"unset uses fallback", "", 0.5, 0.5},
		{"valid float", "0.05", 0, 0.05},
		{"integer parses as float", "3", 0, 3},
		{"negative float", "-1.5", 0, -1.5},
		{"invalid falls back", "abc", 0.5, 0.5},
		{"whitespace only falls back", "   ", 0.5, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_FLOAT", tt.env)
			if got := parseFloatWithDefault("TEST_FLOAT", tt.fallback); got != tt.want {
				t.Errorf("parseFloatWithDefault(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestLoadEpsilonOverrides(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid file", func(t *testing.T) {
		path := filepath.Join(dir, "valid.yaml")
		if err := writeFile(path, `epsilon_overrides:
  "sensor.kitchen_temperature": 0.05
  "sensor.outdoor_humidity": 0.1
`); err != nil {
			t.Fatal(err)
		}
		got, err := loadEpsilonOverrides(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]float64{
			"sensor.kitchen_temperature": 0.05,
			"sensor.outdoor_humidity":    0.1,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("missing file returns error", func(t *testing.T) {
		if _, err := loadEpsilonOverrides(filepath.Join(dir, "does-not-exist.yaml")); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("malformed yaml returns error", func(t *testing.T) {
		path := filepath.Join(dir, "bad.yaml")
		if err := writeFile(path, "epsilon_overrides: [this is not a map"); err != nil {
			t.Fatal(err)
		}
		if _, err := loadEpsilonOverrides(path); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("empty file yields nil map", func(t *testing.T) {
		path := filepath.Join(dir, "empty.yaml")
		if err := writeFile(path, ""); err != nil {
			t.Fatal(err)
		}
		got, err := loadEpsilonOverrides(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil map, got %v", got)
		}
	})
}

func TestLoad(t *testing.T) {
	requiredEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv("HA_BASE_URL", "https://ha.example")
		t.Setenv("HA_TOKEN", "token")
		t.Setenv("PG_DSN", "postgres://user:pass@host:5432/db")
	}

	clearOptionalEnv := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"POLL_INTERVAL", "HTTP_TIMEOUT", "ENTITY_ALLOWLIST", "ENTITY_BLOCKLIST",
			"EPSILON_DEFAULT", "HTTP_LISTEN_ADDR", "CONFIG_FILE",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("happy path uses defaults", func(t *testing.T) {
		requiredEnv(t)
		clearOptionalEnv(t)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.HABaseURL != "https://ha.example" {
			t.Errorf("HABaseURL = %q", cfg.HABaseURL)
		}
		if cfg.PollInterval != time.Minute {
			t.Errorf("PollInterval = %v, want 1m", cfg.PollInterval)
		}
		if cfg.HTTPTimeout != 10*time.Second {
			t.Errorf("HTTPTimeout = %v, want 10s", cfg.HTTPTimeout)
		}
		if !reflect.DeepEqual(cfg.EntityAllowlist, []string{"sensor.*"}) {
			t.Errorf("EntityAllowlist = %v", cfg.EntityAllowlist)
		}
		if cfg.EntityBlocklist != nil {
			t.Errorf("EntityBlocklist = %v, want nil", cfg.EntityBlocklist)
		}
		if cfg.EpsilonDefault != 0 {
			t.Errorf("EpsilonDefault = %v", cfg.EpsilonDefault)
		}
		if cfg.HTTPListenAddr != ":8080" {
			t.Errorf("HTTPListenAddr = %q", cfg.HTTPListenAddr)
		}
		if cfg.EpsilonOverrides != nil {
			t.Errorf("EpsilonOverrides = %v, want nil", cfg.EpsilonOverrides)
		}
	})

	t.Run("missing HA_BASE_URL fails", func(t *testing.T) {
		clearOptionalEnv(t)
		t.Setenv("HA_BASE_URL", "")
		t.Setenv("HA_TOKEN", "token")
		t.Setenv("PG_DSN", "dsn")
		if _, err := Load(); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("missing HA_TOKEN fails", func(t *testing.T) {
		clearOptionalEnv(t)
		t.Setenv("HA_BASE_URL", "https://ha")
		t.Setenv("HA_TOKEN", "")
		t.Setenv("PG_DSN", "dsn")
		if _, err := Load(); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("missing PG_DSN fails", func(t *testing.T) {
		clearOptionalEnv(t)
		t.Setenv("HA_BASE_URL", "https://ha")
		t.Setenv("HA_TOKEN", "token")
		t.Setenv("PG_DSN", "")
		if _, err := Load(); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("non-positive POLL_INTERVAL fails", func(t *testing.T) {
		requiredEnv(t)
		clearOptionalEnv(t)
		t.Setenv("POLL_INTERVAL", "0s")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for POLL_INTERVAL=0s")
		}
	})

	t.Run("CONFIG_FILE loads epsilon overrides", func(t *testing.T) {
		requiredEnv(t)
		clearOptionalEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := writeFile(path, "epsilon_overrides:\n  \"sensor.foo\": 0.25\n"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONFIG_FILE", path)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := cfg.EpsilonOverrides["sensor.foo"]; got != 0.25 {
			t.Errorf("EpsilonOverrides[sensor.foo] = %v, want 0.25", got)
		}
	})

	t.Run("CONFIG_FILE pointing nowhere errors", func(t *testing.T) {
		requiredEnv(t)
		clearOptionalEnv(t)
		t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "nope.yaml"))
		if _, err := Load(); err == nil {
			t.Fatal("expected error for missing config file")
		}
	})

	t.Run("custom values override defaults", func(t *testing.T) {
		requiredEnv(t)
		clearOptionalEnv(t)
		t.Setenv("POLL_INTERVAL", "30s")
		t.Setenv("HTTP_TIMEOUT", "5s")
		t.Setenv("ENTITY_ALLOWLIST", "sensor.*,binary_sensor.*")
		t.Setenv("ENTITY_BLOCKLIST", "sensor.boring_*")
		t.Setenv("EPSILON_DEFAULT", "0.1")
		t.Setenv("HTTP_LISTEN_ADDR", ":9090")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.PollInterval != 30*time.Second {
			t.Errorf("PollInterval = %v", cfg.PollInterval)
		}
		if cfg.HTTPTimeout != 5*time.Second {
			t.Errorf("HTTPTimeout = %v", cfg.HTTPTimeout)
		}
		want := []string{"sensor.*", "binary_sensor.*"}
		if !reflect.DeepEqual(cfg.EntityAllowlist, want) {
			t.Errorf("EntityAllowlist = %v, want %v", cfg.EntityAllowlist, want)
		}
		if !reflect.DeepEqual(cfg.EntityBlocklist, []string{"sensor.boring_*"}) {
			t.Errorf("EntityBlocklist = %v", cfg.EntityBlocklist)
		}
		if cfg.EpsilonDefault != 0.1 {
			t.Errorf("EpsilonDefault = %v", cfg.EpsilonDefault)
		}
		if cfg.HTTPListenAddr != ":9090" {
			t.Errorf("HTTPListenAddr = %q", cfg.HTTPListenAddr)
		}
	})
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
