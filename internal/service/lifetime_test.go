package service

import (
	"testing"
	"time"
)

func TestParseROSDuration(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  time.Duration
	}{
		{"1w2d3h4m5s", 9*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second},
		{"03:04:05", 3*time.Hour + 4*time.Minute + 5*time.Second},
		{"04:05", 4*time.Minute + 5*time.Second},
		{"00:01.5", 1500 * time.Millisecond},
		{"245", 245 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"0s", 0},
		{"0w0d1h", time.Hour},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got, ok := parseROSDuration(tc.input); !ok || got != tc.want {
				t.Fatalf("parseROSDuration(%q) = %v, %v; want %v", tc.input, got, ok, tc.want)
			}
		})
	}
	for _, invalid := range []string{"", "garbage", "-1s", "1h30", "1:2:3:4", "01:60", "01::00", "1:2h", "1.2.3", "999999999999999999999w", "999999999999999:00", "999999999999999999999"} {
		if d, ok := parseROSDuration(invalid); ok {
			t.Errorf("invalid duration %q parsed as %v", invalid, d)
		}
	}
}

func TestSameLifetime(t *testing.T) {
	for _, tc := range []struct {
		configured, actual string
		want               bool
	}{
		{"", "", true}, {"", "0s", false}, {"1h", "", false},
		{"1h", "3600", true}, {"1h", "01:00:00", true}, {"5m", "05:00", true},
		{"1w2d3h4m5s", "788645", true}, {"1h", "59m", false},
		{"unparsed", "unparsed", true}, {"unparsed", "different", false}, {"unparsed", "1h", false},
	} {
		if got := sameLifetime(tc.configured, tc.actual); got != tc.want {
			t.Errorf("sameLifetime(%q, %q) = %v", tc.configured, tc.actual, got)
		}
	}
}

func TestListRenewalDelay(t *testing.T) {
	for _, tc := range []struct {
		timeout string
		want    time.Duration
	}{
		{"", 0}, {"invalid", 0}, {"0s", 0}, {"-1s", 0}, {"1ns", 0},
		{"2s", time.Second}, {"01:00:00", 30 * time.Minute}, {"1w", 84 * time.Hour},
		{"00:00.15", 100 * time.Millisecond}, {"00:00.04", 30 * time.Millisecond}, {"2ns", time.Nanosecond},
	} {
		if got := listRenewalDelay(tc.timeout); got != tc.want {
			t.Errorf("listRenewalDelay(%q) = %v, want %v", tc.timeout, got, tc.want)
		} else if lifetime, ok := parseROSDuration(tc.timeout); ok && got > 0 && got >= lifetime {
			t.Errorf("renewal %v is not before expiry %v", got, lifetime)
		}
	}
}

func TestDynamicListRecheckDelay(t *testing.T) {
	for _, tc := range []struct {
		timeout      string
		verify, want time.Duration
	}{
		{"", 0, time.Minute},
		{"unknown", 10 * time.Minute, time.Minute},
		{"", 10 * time.Second, 10 * time.Second},
		{"0s", 0, time.Minute},
		{"2s", time.Minute, time.Second},
	} {
		if got := dynamicListRecheckDelay(tc.timeout, tc.verify); got != tc.want {
			t.Errorf("dynamicListRecheckDelay(%q, %v) = %v, want %v", tc.timeout, tc.verify, got, tc.want)
		}
	}
}
