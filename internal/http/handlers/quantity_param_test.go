package handlers

import "testing"

func TestParseQuantityParam(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  float64
	}{
		{"empty defaults to 1", "", 1},
		{"whole number", "5", 5},
		{"decimal (perfume refill by the ml)", "1.5", 1.5},
		{"small decimal", "0.5", 0.5},
		{"zero falls back to 1", "0", 1},
		{"negative falls back to 1", "-2", 1},
		{"garbage falls back to 1", "abc", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQuantityParam(tc.input)
			if got != tc.want {
				t.Errorf("parseQuantityParam(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
