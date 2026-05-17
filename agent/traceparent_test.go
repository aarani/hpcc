package main

import "testing"

func TestTraceIDFromTraceparent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"valid", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"all-zero trace id rejected", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""},
		{"too few parts", "00-4bf92f3577b34da6a3ce929d0e0e4736", ""},
		{"trace id wrong length", "00-deadbeef-00f067aa0ba902b7-01", ""},
		{"non-hex in trace id", "00-4bf92f3577b34da6a3ce929d0e0e473G-00f067aa0ba902b7-01", ""},
		{"uppercase hex rejected (W3C is lowercase)", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := traceIDFromTraceparent(tc.in); got != tc.want {
				t.Fatalf("traceIDFromTraceparent(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
