package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	HABaseURL       string
	HAToken         string
	PGDSN           string
	PollInterval    time.Duration
	HTTPTimeout     time.Duration
	EntityAllowlist []string
	EntityBlocklist []string
}

func Load() (Config, error) {
	cfg := Config{
		HABaseURL:       strings.TrimSpace(os.Getenv("HA_BASE_URL")),
		HAToken:         strings.TrimSpace(os.Getenv("HA_TOKEN")),
		PGDSN:           strings.TrimSpace(os.Getenv("PG_DSN")),
		PollInterval:    parseDurationWithDefault("POLL_INTERVAL", time.Minute),
		HTTPTimeout:     parseDurationWithDefault("HTTP_TIMEOUT", 10*time.Second),
		EntityAllowlist: splitCSVWithDefault(os.Getenv("ENTITY_ALLOWLIST"), []string{"sensor.*"}),
		EntityBlocklist: splitCSVWithDefault(os.Getenv("ENTITY_BLOCKLIST"), nil),
	}

	if cfg.HABaseURL == "" {
		return Config{}, fmt.Errorf("HA_BASE_URL is required")
	}
	if cfg.HAToken == "" {
		return Config{}, fmt.Errorf("HA_TOKEN is required")
	}
	if cfg.PGDSN == "" {
		return Config{}, fmt.Errorf("PG_DSN is required")
	}
	if cfg.PollInterval <= 0 {
		return Config{}, fmt.Errorf("POLL_INTERVAL must be > 0")
	}

	return cfg, nil
}

func parseDurationWithDefault(envKey string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func splitCSVWithDefault(raw string, fallback []string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if fallback == nil {
			return nil
		}
		out := make([]string, len(fallback))
		copy(out, fallback)
		return out
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
