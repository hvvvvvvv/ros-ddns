// Package redact removes configured credentials from diagnostic text.
package redact

import (
	"encoding/json"
	"net/url"
	"strings"

	"ros-ddns/internal/config"
)

// Redactor is immutable and safe for concurrent use. Its zero value is a no-op.
type Redactor struct {
	secrets []secretVariant
}

type secretVariant struct {
	value   string
	bounded bool // determined by the original secret's byte length
}

// FromConfig collects RouterOS, API, URL userinfo and DNS provider credentials.
func FromConfig(cfg *config.Config) *Redactor {
	if cfg == nil {
		return New(nil)
	}
	secrets := []string{cfg.RouterOS.Password, cfg.API.Token}
	if u, err := url.Parse(cfg.RouterOS.URL); err == nil && u.User != nil {
		password, _ := u.User.Password()
		secrets = append(secrets, password)
	}
	for _, p := range cfg.DNS.Providers {
		secrets = append(secrets, p.AccessKeyID, p.AccessKeySecret, p.SecurityToken)
	}
	return New(secrets)
}

// New deduplicates raw, JSON-string-escaped and URL-query-escaped secrets.
// Short-secret boundaries depend on the original secret's byte length.
func New(secrets []string) *Redactor {
	r := &Redactor{}
	seen := make(map[string]int)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		bounded := len(secret) < 8
		for _, variant := range []string{secret, string(encoded[1 : len(encoded)-1]), url.QueryEscape(secret)} {
			if i, ok := seen[variant]; ok {
				// A variant may also be a long original secret. Preserve its
				// substring matching regardless of the discovery order.
				r.secrets[i].bounded = r.secrets[i].bounded && bounded
			} else {
				seen[variant] = len(r.secrets)
				r.secrets = append(r.secrets, secretVariant{value: variant, bounded: bounded})
			}
		}
	}
	return r
}

// Replace hides matching credentials while preserving existing redaction markers.
// A nil receiver leaves the message unchanged.
func (r *Redactor) Replace(message string) string {
	if r == nil || len(r.secrets) == 0 {
		return message
	}
	// Existing markers are opaque, including when a configured secret is part
	// of the marker itself. Errors can pass through redaction more than once.
	parts := strings.Split(message, "[redacted]")
	for i, part := range parts {
		parts[i] = r.replacePart(part)
	}
	return strings.Join(parts, "[redacted]")
}

func (r *Redactor) replacePart(message string) string {
	// Match against the original text and mark the union of matching spans.
	// This hides overlapping secrets regardless of their discovery order.
	var hidden []bool
	for _, secret := range r.secrets {
		for offset := 0; offset < len(message); {
			n := strings.Index(message[offset:], secret.value)
			if n < 0 {
				break
			}
			start := offset + n
			end := start + len(secret.value)
			offset = start + 1
			if secret.bounded && ((start > 0 && asciiAlphanumeric(message[start-1])) ||
				(end < len(message) && asciiAlphanumeric(message[end]))) {
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
	for i := 0; i < len(message); i++ {
		if !hidden[i] {
			out.WriteByte(message[i])
		} else if i == 0 || !hidden[i-1] {
			out.WriteString("[redacted]")
		}
	}
	return out.String()
}

func asciiAlphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
