package main

import "testing"

func TestParseFallbackChoice(t *testing.T) {
	cases := []struct {
		in   string
		want fallbackChoice
	}{
		{"", fallbackContinue},
		{"c", fallbackContinue},
		{"C", fallbackContinue},
		{"  c\n", fallbackContinue},
		{"a", fallbackAbort},
		{"abort", fallbackAbort},
		{"n", fallbackAbort},
		{"anything", fallbackAbort},
	}
	for _, tc := range cases {
		if got := parseFallbackChoice(tc.in); got != tc.want {
			t.Errorf("parseFallbackChoice(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
