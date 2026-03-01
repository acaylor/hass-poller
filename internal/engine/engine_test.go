package engine

import "testing"

func TestShouldWrite(t *testing.T) {
	tests := []struct {
		name             string
		current          float64
		last             float64
		epsilon          float64
		firstObservation bool
		want             bool
	}{
		// First observation always writes
		{"first observation", 20.5, 0, 0, true, true},
		{"first observation with epsilon", 20.5, 0, 1.0, true, true},

		// Strict equality (epsilon=0)
		{"same value eps=0", 20.5, 20.5, 0, false, false},
		{"different value eps=0", 20.5, 20.4, 0, false, true},
		{"tiny change eps=0", 20.5, 20.500000001, 0, false, true},

		// With epsilon threshold
		{"change below epsilon", 20.5, 20.48, 0.05, false, false},
		{"change at epsilon boundary", 20.5, 20.0, 0.5, false, false},
		{"change above epsilon", 20.5, 20.0, 0.05, false, true},
		{"negative change below epsilon", 20.45, 20.48, 0.05, false, false},
		{"negative change above epsilon", 20.0, 20.5, 0.05, false, true},

		// Zero values
		{"zero to zero", 0, 0, 0, false, false},
		{"zero to nonzero", 0.1, 0, 0, false, true},
		{"zero first observation", 0, 0, 0, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldWrite(tt.current, tt.last, tt.epsilon, tt.firstObservation)
			if got != tt.want {
				t.Errorf("ShouldWrite(%v, %v, %v, %v) = %v, want %v",
					tt.current, tt.last, tt.epsilon, tt.firstObservation, got, tt.want)
			}
		})
	}
}

func TestParseNumericState(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  float64
		wantOK bool
	}{
		{"integer", "42", 42, true},
		{"float", "20.5", 20.5, true},
		{"negative", "-3.14", -3.14, true},
		{"zero", "0", 0, true},
		{"whitespace", "  20.5  ", 20.5, true},

		// Rejected values
		{"unknown", "unknown", 0, false},
		{"unavailable", "unavailable", 0, false},
		{"Unknown uppercase", "Unknown", 0, false},
		{"empty", "", 0, false},
		{"text", "on", 0, false},
		{"mixed", "20.5°C", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseNumericState(tt.raw)
			if ok != tt.wantOK {
				t.Errorf("parseNumericState(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
				return
			}
			if ok && got != tt.want {
				t.Errorf("parseNumericState(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
