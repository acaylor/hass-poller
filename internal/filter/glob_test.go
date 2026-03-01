package filter

import "testing"

func TestGlobFilter(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
		blocklist []string
		entityID  string
		want      bool
	}{
		// Allowlist only
		{"allow sensor glob", []string{"sensor.*"}, nil, "sensor.temperature", true},
		{"reject non-sensor", []string{"sensor.*"}, nil, "binary_sensor.door", false},
		{"allow multiple globs", []string{"sensor.*", "binary_sensor.*"}, nil, "binary_sensor.door", true},
		{"empty allowlist allows all", nil, nil, "anything.here", true},

		// Blocklist only
		{"blocklist rejects match", nil, []string{"sensor.energy_*"}, "sensor.energy_total", false},
		{"blocklist allows non-match", nil, []string{"sensor.energy_*"}, "sensor.temperature", true},

		// Both
		{"blocklist overrides allowlist", []string{"sensor.*"}, []string{"sensor.energy_*"}, "sensor.energy_total", false},
		{"allowed and not blocked", []string{"sensor.*"}, []string{"sensor.energy_*"}, "sensor.temperature", true},
		{"not in allowlist ignored by blocklist", []string{"sensor.*"}, []string{"binary_sensor.*"}, "binary_sensor.door", false},

		// Edge cases
		{"exact match glob", []string{"sensor.temperature"}, nil, "sensor.temperature", true},
		{"no match exact", []string{"sensor.temperature"}, nil, "sensor.humidity", false},
		{"wildcard in middle", []string{"sensor.*_temperature"}, nil, "sensor.kitchen_temperature", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewGlobFilter(tt.allowlist, tt.blocklist)
			got := f.Allowed(tt.entityID)
			if got != tt.want {
				t.Errorf("Allowed(%q) = %v, want %v", tt.entityID, got, tt.want)
			}
		})
	}
}
