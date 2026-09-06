package config

import (
	"fmt"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	HABaseURL        string
	HAToken          string
	PGDSN            string
	PollInterval     time.Duration
	HTTPTimeout      time.Duration
	EntityAllowlist  []string
	EntityBlocklist  []string
	EpsilonDefault   float64
	EpsilonOverrides map[string]float64
	HTTPListenAddr   string
}

func Load() (Config, error) {
	cfg := Config{
		HABaseURL:       strings.TrimSpace(os.Getenv("HA_BASE_URL")),
		HAToken:         strings.TrimSpace(os.Getenv("HA_TOKEN")),
		PGDSN:           strings.TrimSpace(os.Getenv("PG_DSN")),
		EntityAllowlist: splitCSVWithDefault(os.Getenv("ENTITY_ALLOWLIST"), []string{"sensor.*"}),
		EntityBlocklist: splitCSVWithDefault(os.Getenv("ENTITY_BLOCKLIST"), nil),
		HTTPListenAddr:  stringWithDefault(os.Getenv("HTTP_LISTEN_ADDR"), ":8080"),
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
	var err error
	if cfg.PollInterval, err = durationFromEnv("POLL_INTERVAL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.HTTPTimeout, err = durationFromEnv("HTTP_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if raw := strings.TrimSpace(os.Getenv("EPSILON_DEFAULT")); raw != "" {
		cfg.EpsilonDefault, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, fmt.Errorf("EPSILON_DEFAULT: %w", err)
		}
	}
	if err := validateEpsilon("EPSILON_DEFAULT", cfg.EpsilonDefault); err != nil {
		return Config{}, err
	}
	for key, patterns := range map[string][]string{
		"ENTITY_ALLOWLIST": cfg.EntityAllowlist, "ENTITY_BLOCKLIST": cfg.EntityBlocklist,
	} {
		for _, pattern := range patterns {
			if _, err := path.Match(pattern, ""); err != nil {
				return Config{}, fmt.Errorf("%s: invalid glob %q: %w", key, pattern, err)
			}
		}
	}

	if configFile := strings.TrimSpace(os.Getenv("CONFIG_FILE")); configFile != "" {
		overrides, err := loadEpsilonOverrides(configFile)
		if err != nil {
			return Config{}, fmt.Errorf("load config file: %w", err)
		}
		cfg.EpsilonOverrides = overrides
	}

	return cfg, nil
}

func durationFromEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be > 0", key)
	}
	return d, nil
}

func stringWithDefault(val, fallback string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return fallback
	}
	return val
}

func validateEpsilon(name string, value float64) error {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s must be finite and >= 0", name)
	}
	return nil
}

type configFile struct {
	EpsilonOverrides map[string]float64 `yaml:"epsilon_overrides"`
}

func loadEpsilonOverrides(path string) (map[string]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf configFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, err
	}
	for entityID, epsilon := range cf.EpsilonOverrides {
		if err := validateEpsilon("epsilon_overrides["+entityID+"]", epsilon); err != nil {
			return nil, err
		}
	}
	return cf.EpsilonOverrides, nil
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
	if len(out) == 0 {
		return fallback
	}
	return out
}
