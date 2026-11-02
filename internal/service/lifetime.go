package service

import (
	"strconv"
	"strings"
	"time"
)

func sameLifetime(configured, actual string) bool {
	// An absent timeout is static; even a zero duration is a dynamic timeout.
	if configured == "" || actual == "" {
		return configured == actual
	}
	want, wantOK := parseROSDuration(configured)
	got, gotOK := parseROSDuration(actual)
	if wantOK && gotOK {
		return want == got
	}
	return configured == actual
}

// parseROSDuration accepts RouterOS unit, clock and plain-second formats.
// Check overflow so a large/malformed timeout cannot become a short timer.
//
// This parser is deliberately more permissive than config's strict
// validation: it interprets values returned by RouterOS during reconciliation
// (for example clock formats the configuration layer rejects). Keep the two
// behaviours separate rather than merging the parsers.
func parseROSDuration(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(value); err == nil {
		return d, d >= 0
	}
	const largest = time.Duration(1<<63 - 1)
	if strings.Contains(value, ":") {
		parts := strings.Split(value, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return 0, false
		}
		units := []string{"h", "m", "s"}[3-len(parts):]
		scales := []time.Duration{time.Hour, time.Minute, time.Second}[3-len(parts):]
		var total time.Duration
		for i, part := range parts {
			if !decimalNumber(part, i == len(parts)-1) {
				return 0, false
			}
			d, err := time.ParseDuration(part + units[i])
			if err != nil || d > largest-total || (i > 0 && d >= 60*scales[i]) {
				return 0, false
			}
			total += d
		}
		return total, true
	}
	if decimalNumber(value, true) {
		d, err := time.ParseDuration(value + "s")
		return d, err == nil
	}
	units := map[byte]time.Duration{'w': 7 * 24 * time.Hour, 'd': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}
	var total time.Duration
	for len(value) > 0 {
		i := 0
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		if i == 0 || i == len(value) {
			return 0, false
		}
		unit, ok := units[value[i]]
		n, err := strconv.ParseUint(value[:i], 10, 63)
		if !ok || err != nil || n > uint64((largest-total)/unit) {
			return 0, false
		}
		total += time.Duration(n) * unit
		value = value[i+1:]
	}
	return total, true
}

func decimalNumber(s string, fraction bool) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] == '.' && fraction && i > 0 && i < len(s)-1 {
			fraction = false
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func listRenewalDelay(timeout string) time.Duration {
	lifetime, ok := parseROSDuration(timeout)
	if !ok || lifetime <= time.Nanosecond {
		return 0
	}
	// Normally renew halfway through the lifetime. A 100ms floor avoids very
	// tight timers, but leave at least a quarter of even a short lifetime.
	return min(max(lifetime/2, 100*time.Millisecond), lifetime-max(lifetime/4, time.Nanosecond))
}

// Dynamic entries with an unknown or already elapsed lifetime still need a
// bounded recheck, even when periodic full verification is disabled.
func dynamicListRecheckDelay(timeout string, verifyInterval time.Duration) time.Duration {
	if delay := listRenewalDelay(timeout); delay > 0 {
		return delay
	}
	if verifyInterval > 0 {
		return min(time.Minute, verifyInterval)
	}
	return time.Minute
}
