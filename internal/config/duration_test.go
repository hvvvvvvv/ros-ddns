package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseROSDuration(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  time.Duration
	}{
		{"1h30m", 90 * time.Minute},
		{"1w2d3h4m5s", 788645 * time.Second},
		{"00:10:00", 10 * time.Minute},
		{"10:00", 10 * time.Minute},
		{"1:2:3", time.Hour + 2*time.Minute + 3*time.Second},
		{"99:00:00", 99 * time.Hour},
		{"59:59", 3599 * time.Second},
		{"00:59:59.999999999", time.Hour - time.Nanosecond},
		{"00:00.000000001", time.Nanosecond},
		{"00:01.5", 1500 * time.Millisecond},
		{"60s", time.Minute},
		{"245", 245 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"00060", time.Minute},
		{"0", 0},
		{"0s", 0},
		{"0.0", 0},
		{"00:00", 0},
		{"00:00:00", 0},
		{"0w0d0h0m0s", 0},
		{"0w0d1h", time.Hour},
		{" 1h30m ", 90 * time.Minute},
		{"35w3d13h13m55s", 21474835 * time.Second},
		{"35w3d13h13m56s", 21474836 * time.Second},
		{"21474836s", 21474836 * time.Second},
		{"21474836", 21474836 * time.Second},
		{"21474836.000000000", 21474836 * time.Second},
		{"5965:13:56", 21474836 * time.Second},
		{"5965:13:55.999999999", 21474836*time.Second - time.Nanosecond},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseROSDuration(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("parseROSDuration(%q) = %v, %v; want %v", tc.input, got, err, tc.want)
			}
		})
	}
}

func TestParseROSDurationRejectsInvalid(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "must not be empty"},
		{" \t", "must not be empty"},
		{"36w", "must not exceed"},
		{"35w4d", "must not exceed"},
		{"35w3d13h13m57s", "must not exceed"},
		{"21474837", "must not exceed"},
		{"21474836.000000001", "must not exceed"},
		{"5965:13:56.000000001", "must not exceed"},
		{"5965:14:00", "must not exceed"},
		{"5970:00:00", "must not exceed"},
		{"999999999999999999999w", "must not exceed"},
		{"999999999999999999999", "must not exceed"},
		{"999999999999999999999:00:00", "must not exceed"},
		{"00:00:999999999999999999999", "must not exceed"},
		{"18446744073709551615s", "must not exceed"},
		{"18446744073709551616s", "must not exceed"},
		{"2562048h", "must not exceed"},
		{"01:60", "minutes and seconds"},
		{"00:00:60", "minutes and seconds"},
		{"00:60:00", "minutes and seconds"},
		{"60:00", "minutes and seconds"},
		{"99:00", "minutes and seconds"},
		{"00:60.1", "minutes and seconds"},
		{":00", "invalid RouterOS duration"},
		{"00:", "invalid RouterOS duration"},
		{"01::00", "invalid RouterOS duration"},
		{"1:2:3:4", "invalid RouterOS duration"},
		{"1.5:00", "invalid RouterOS duration"},
		{"1:2h", "invalid RouterOS duration"},
		{"00:-1", "invalid RouterOS duration"},
		{"00:+1", "invalid RouterOS duration"},
		{"00: 1", "invalid RouterOS duration"},
		{"00:1.", "invalid RouterOS duration"},
		{"00:.5", "invalid RouterOS duration"},
		{"00:1.2.3", "invalid RouterOS duration"},
		{"00:0.0000000001", "invalid RouterOS duration"},
		{"0.0000000001", "invalid RouterOS duration"},
		{"21474836.0000000001", "invalid RouterOS duration"},
		{"1h30", "invalid RouterOS duration"},
		{"1w2x", "invalid RouterOS duration"},
		{"1h 30m", "invalid RouterOS duration"},
		{"h", "invalid RouterOS duration"},
		{"1H", "invalid RouterOS duration"},
		{"1ms", "invalid RouterOS duration"},
		{"1.5h", "invalid RouterOS duration"},
		{"-1s", "invalid RouterOS duration"},
		{"+1s", "invalid RouterOS duration"},
		{"1e3", "invalid RouterOS duration"},
		{"１２s", "invalid RouterOS duration"},
		{"sensitive-timeout", "invalid RouterOS duration"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseROSDuration(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseROSDuration(%q) = %v, %v; want error containing %q", tc.input, got, err, tc.want)
			}
			if got != 0 {
				t.Errorf("invalid duration returned %v, want zero", got)
			}
			if strings.Contains(err.Error(), "sensitive-timeout") {
				t.Errorf("parser error leaks input: %v", err)
			}
		})
	}
}
