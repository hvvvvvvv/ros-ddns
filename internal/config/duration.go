package config

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Configuration timeouts are validated strictly for RouterOS address-list
// limits and nanosecond precision. The service lifetime parser is deliberately
// more permissive when interpreting remote RouterOS values for reconciliation.
// RouterOS address-list timeouts are limited to 35w3d13h13m56s, inclusive.
const maxROSTimeout time.Duration = 35*7*24*time.Hour + 3*24*time.Hour + 13*time.Hour + 13*time.Minute + 56*time.Second

var (
	errROSDurationFormat = errors.New("invalid RouterOS duration (use e.g. 1h30m, 00:10:00, or seconds)")
	errROSDurationRange  = errors.New("must not exceed RouterOS maximum 35w3d13h13m56s")
)

// parseROSDuration accepts non-negative unit, clock and plain-second durations.
// Empty config timeouts denote static entries and are handled by the caller.
func parseROSDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("duration must not be empty")
	}
	if strings.Contains(value, ":") {
		parts := strings.Split(value, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return 0, errROSDurationFormat
		}
		units := []string{"h", "m", "s"}[3-len(parts):]
		scales := []time.Duration{time.Hour, time.Minute, time.Second}[3-len(parts):]
		var total time.Duration
		for i, part := range parts {
			if !rosDecimalNumber(part, i == len(parts)-1) {
				return 0, errROSDurationFormat
			}
			d, err := time.ParseDuration(part + units[i])
			if err != nil {
				return 0, errROSDurationRange
			}
			if scales[i] != time.Hour && d >= 60*scales[i] {
				return 0, errors.New("clock minutes and seconds must be less than 60")
			}
			if d > maxROSTimeout-total {
				return 0, errROSDurationRange
			}
			total += d
		}
		return total, nil
	}
	if rosDecimalNumber(value, true) {
		d, err := time.ParseDuration(value + "s")
		if err != nil || d > maxROSTimeout {
			return 0, errROSDurationRange
		}
		return d, nil
	}
	units := map[byte]time.Duration{'w': 7 * 24 * time.Hour, 'd': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}
	var total time.Duration
	for len(value) > 0 {
		i := 0
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		if i == 0 || i == len(value) {
			return 0, errROSDurationFormat
		}
		unit, ok := units[value[i]]
		if !ok {
			return 0, errROSDurationFormat
		}
		n, err := strconv.ParseUint(value[:i], 10, 64)
		// Bound the component before multiplying or adding to avoid overflow.
		if err != nil || n > uint64((maxROSTimeout-total)/unit) {
			return 0, errROSDurationRange
		}
		total += time.Duration(n) * unit
		value = value[i+1:]
	}
	return total, nil
}

func rosDecimalNumber(s string, fraction bool) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] == '.' && fraction && i > 0 && i < len(s)-1 {
			// time.Duration stores nanoseconds; do not silently truncate precision.
			if len(s)-i-1 > 9 {
				return false
			}
			fraction = false
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
