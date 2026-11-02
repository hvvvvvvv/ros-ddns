package config

import (
	"errors"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const envRedactionMarker = "[redacted]"

func redactEnvError(err error, values map[string]struct{}) error {
	if err == nil || len(values) == 0 {
		return err
	}
	message := err.Error()
	redacted := redactEnvValues(message, values)
	if redacted == message {
		return err
	}
	// Do not retain the original error via Unwrap: it can contain the secrets.
	return errors.New(redacted)
}

// redactEnvValues matches against the original message, longest first, then
// replaces the union of all matching spans. Overlaps cannot leave secret tails
// behind, and replacement text is never searched again.
func redactEnvValues(message string, values map[string]struct{}) string {
	// The bool records whether a pattern needs ASCII token boundaries. Escaping
	// a short value must not accidentally turn it into an unbounded pattern.
	patterns := make(map[string]bool)
	add := func(value string, bounded bool) {
		if value == "" {
			return
		}
		if previous, ok := patterns[value]; ok {
			bounded = bounded && previous
		}
		patterns[value] = bounded
	}
	variants := envValueVariants(values)
	for value := range variants {
		if value == "" {
			continue
		}
		bounded := len(value) < 8
		add(value, bounded)
		// Validators and custom unmarshalers use %q, which escapes quotes,
		// backslashes, newlines, and non-printing bytes.
		quoted := strconv.Quote(value)
		add(quoted[1:len(quoted)-1], bounded)
		// yaml.v3's type errors print only the first seven bytes of a scalar
		// longer than ten bytes. Redact the preview, not arbitrary prefixes.
		if len(value) > 10 {
			add(value[:7]+"...", false)
		}
	}

	// Parsers can quote a fragment as well as the full value, e.g. a duration's
	// unknown unit or the invalid suffix of a CIDR address. Keep the surrounding
	// explanation while removing those fragments of an expanded value too. Only
	// consider values also quoted in full, so unrelated quoted words that merely
	// occur inside an absent secret do not cause redaction.
	for offset := 0; offset < len(message); {
		n := strings.IndexByte(message[offset:], '"')
		if n < 0 {
			break
		}
		offset += n
		quoted, err := strconv.QuotedPrefix(message[offset:])
		if err != nil {
			offset++
			continue
		}
		offset += len(quoted)
		fragment, err := strconv.Unquote(quoted)
		if err != nil || fragment == "" {
			continue
		}
		for value := range variants {
			if strings.Contains(value, fragment) && strings.Contains(message, strconv.Quote(value)) {
				add(quoted[1:len(quoted)-1], len(fragment) < 8)
				break
			}
		}
	}

	ordered := make([]string, 0, len(patterns))
	for value := range patterns {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) == len(ordered[j]) {
			return ordered[i] < ordered[j]
		}
		return len(ordered[i]) > len(ordered[j])
	})
	var hidden []bool
	for _, value := range ordered {
		for offset := 0; offset < len(message); {
			n := strings.Index(message[offset:], value)
			if n < 0 {
				break
			}
			start := offset + n
			end := start + len(value)
			offset = start + 1 // Include overlapping occurrences.
			if patterns[value] && ((start > 0 && envASCIIAlphanumeric(message[start-1])) ||
				(end < len(message) && envASCIIAlphanumeric(message[end]))) {
				continue
			}
			if hidden == nil {
				hidden = make([]bool, len(message))
			}
			for i := start; i < end; i++ {
				hidden[i] = true
			}
		}
	}
	if hidden == nil {
		return message
	}
	var out strings.Builder
	for i := 0; i < len(message); {
		if strings.HasPrefix(message[i:], envRedactionMarker) {
			// Preserve existing markers even if a value matches inside one.
			out.WriteString(envRedactionMarker)
			i += len(envRedactionMarker)
		} else if !hidden[i] {
			out.WriteByte(message[i])
			i++
		} else {
			out.WriteString(envRedactionMarker)
			for i < len(message) && hidden[i] && !strings.HasPrefix(message[i:], envRedactionMarker) {
				i++
			}
		}
	}
	return out.String()
}

func envASCIIAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// envValueVariants includes the representations emitted by Normalize and the
// parsers called by Validate. Keep these in sync when adding new diagnostics.
func envValueVariants(values map[string]struct{}) map[string]struct{} {
	variants := make(map[string]struct{})
	for value := range values {
		for _, variant := range []string{value, strings.TrimSpace(value), strings.ToLower(value), lowerTrim(value)} {
			variants[variant] = struct{}{}
			variants[trimRightDot(variant)] = struct{}{}
			variants[trimRightSlash(variant)] = struct{}{}
			// URL validation can report just the lowercased scheme.
			if u, err := url.Parse(variant); err == nil && u.Scheme != "" {
				variants[u.Scheme] = struct{}{}
			}
			// A family mismatch reports the canonical, unmasked prefix.
			if prefix, err := netip.ParsePrefix(variant); err == nil {
				variants[prefix.String()] = struct{}{}
			}
			// eui64.ParseMAC inserts colons in a 12-byte value before parsing;
			// net.ParseMAC can then echo that spelling in its error.
			if len(variant) == 12 && !strings.ContainsAny(variant, ":-.") {
				var mac strings.Builder
				for i := 0; i < len(variant); i += 2 {
					if i > 0 {
						mac.WriteByte(':')
					}
					mac.WriteString(variant[i : i+2])
				}
				variants[mac.String()] = struct{}{}
			}
		}
	}
	delete(variants, "")
	return variants
}
