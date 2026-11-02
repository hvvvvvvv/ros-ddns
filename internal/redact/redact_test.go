package redact

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"ros-ddns/internal/config"
)

func TestNewOwnsAndDeduplicatesSecrets(t *testing.T) {
	secrets := []string{"long-secret", "long-secret", "", "p"}
	r := New(secrets)
	secrets[0], secrets[3] = "changed", "changed"
	if got := r.Replace("long-secret p poll"); got != "[redacted] [redacted] poll" {
		t.Fatalf("Replace after input mutation = %q", got)
	}
	if len(r.secrets) != 2 {
		t.Fatalf("got %d variants, want 2 deduplicated variants", len(r.secrets))
	}
}

func TestEmptyRedactor(t *testing.T) {
	for _, r := range []*Redactor{nil, {}, New(nil), FromConfig(nil)} {
		if got := r.Replace("unchanged [redacted]"); got != "unchanged [redacted]" {
			t.Fatalf("empty redactor = %q", got)
		}
	}
}

func TestRedactorPreservesWordsContainingShortSecrets(t *testing.T) {
	cfg := &config.Config{}
	cfg.RouterOS.Password = "p"
	cfg.API.Token = ""
	cfg.DNS.Providers = map[string]config.ProviderConfig{
		"ali": {AccessKeyID: "id", AccessKeySecret: "secret"},
	}

	const message = "poll ipv4 addresses: RouterOS GET http://127.0.0.1:1/rest/ip/address"
	if got := FromConfig(cfg).Replace(message); got != message {
		t.Fatalf("short secrets corrupted the message:\n got: %s\nwant: %s", got, message)
	}
}

func TestRedactorShortTokenBoundaries(t *testing.T) {
	for _, secret := range []string{"p", "id", "passw0d", "passw0rd"} {
		t.Run(secret, func(t *testing.T) {
			c := &config.Config{}
			c.RouterOS.Password = secret
			r := FromConfig(c)
			for _, bounds := range [][2]string{{"", ""}, {"password=", ""}, {`"`, `"`}, {"[", "]"}, {"_", "_"}, {"中文", "中文"}} {
				message := bounds[0] + secret + bounds[1]
				if got := r.Replace(message); got != bounds[0]+"[redacted]"+bounds[1] {
					t.Errorf("bounded secret was not redacted: %q => %q", message, got)
				}
			}
			if len(secret) < 8 {
				for _, message := range []string{"x" + secret, secret + "x", "0" + secret, secret + "0", "A" + secret + "Z"} {
					if got := r.Replace(message); got != message {
						t.Errorf("unbounded short secret corrupted text: %q => %q", message, got)
					}
				}
			}
		})
	}
}

func TestRedactorOverlappingSecrets(t *testing.T) {
	c := &config.Config{}
	c.RouterOS.Password = "abcdefgh"
	c.API.Token = "abcdefgh-more"
	c.DNS.Providers = map[string]config.ProviderConfig{
		"test": {AccessKeyID: "more", AccessKeySecret: "cdefgh-more-tail", SecurityToken: "redact"},
	}
	message := "abcdefgh-more abcdefgh-more-tail [more] redact"
	if got, want := FromConfig(c).Replace(message), "[redacted] [redacted] [[redacted]] [redacted]"; got != want {
		t.Fatalf("overlap redaction = %q, want %q", got, want)
	}
}

func TestRedactorReplacesLongSecrets(t *testing.T) {
	const (
		password = "super-secret-password"
		akSecret = "very-long-access-key-secret"
		token    = "api-token-value-long"
	)
	cfg := &config.Config{}
	cfg.RouterOS.Password = password
	cfg.API.Token = token
	cfg.DNS.Providers = map[string]config.ProviderConfig{
		"ali": {AccessKeySecret: akSecret},
	}

	got := FromConfig(cfg).Replace("failed with " + password + ", " + akSecret + ", " + token)
	for _, secret := range []string{password, akSecret, token} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted message still contains %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("redacted message lacks marker: %s", got)
	}
}

func TestRedactorConfiguredCredentialVariants(t *testing.T) {
	c := &config.Config{}
	c.RouterOS.Password = "router\"\\\npassword"
	c.API.Token = "api token<&>"
	urlPassword := "url\"\\\npassword"
	c.RouterOS.URL = "https://" + url.UserPassword("user", urlPassword).String() + "@router.example"
	p := config.ProviderConfig{AccessKeyID: "access\"key", AccessKeySecret: "secret\\key", SecurityToken: "session\ntoken"}
	c.DNS.Providers = map[string]config.ProviderConfig{"test": p}
	r := FromConfig(c)
	for _, secret := range []string{c.RouterOS.Password, c.API.Token, urlPassword, p.AccessKeyID, p.AccessKeySecret, p.SecurityToken} {
		encoded, _ := json.Marshal(secret)
		for _, variant := range []string{secret, string(encoded[1 : len(encoded)-1]), url.QueryEscape(secret)} {
			if got := r.Replace("poll credential=" + variant); got != "poll credential=[redacted]" {
				t.Errorf("credential variant was not redacted: %q => %q", variant, got)
			}
		}
	}
}

func TestRedactorVariantBoundariesAndExistingMarkers(t *testing.T) {
	c := &config.Config{}
	c.RouterOS.Password = `"`
	c.API.Token = "redacted"
	r := FromConfig(c)
	for _, variant := range []string{`\"`, "%22"} {
		if got := r.Replace("credential=" + variant); got != "credential=[redacted]" {
			t.Errorf("bounded variant was not redacted: %q", got)
		}
		if got, want := r.Replace("x"+variant+"y"), "x"+variant+"y"; got != want {
			t.Errorf("short variant corrupted surrounding text: got %q want %q", got, want)
		}
	}
	if got := r.Replace("poll [redacted] redacted"); got != "poll [redacted] [redacted]" || r.Replace(got) != got {
		t.Fatalf("redaction markers were replaced again: %q", got)
	}
}

func TestRedactorVariantBoundariesUseOriginalSecretLength(t *testing.T) {
	for _, secret := range []string{"<&>", "p", "long-secret<&>"} {
		t.Run(secret, func(t *testing.T) {
			c := &config.Config{}
			c.RouterOS.Password = secret
			r := FromConfig(c)
			encoded, _ := json.Marshal(secret)
			for _, variant := range []string{secret, string(encoded[1 : len(encoded)-1]), url.QueryEscape(secret)} {
				for _, bounds := range [][2]string{{"", ""}, {"credential=", ""}, {`"`, `"`}} {
					message := bounds[0] + variant + bounds[1]
					if got, want := r.Replace(message), bounds[0]+"[redacted]"+bounds[1]; got != want {
						t.Errorf("standalone variant: Replace(%q) = %q, want %q", message, got, want)
					}
				}
				for _, bounds := range [][2]string{{"prefix", "suffix"}, {"prefix", ""}, {"", "suffix"}} {
					message := bounds[0] + variant + bounds[1]
					want := message
					if len(secret) >= 8 {
						want = bounds[0] + "[redacted]" + bounds[1]
					}
					if got := r.Replace(message); got != want {
						t.Errorf("embedded variant: Replace(%q) = %q, want %q", message, got, want)
					}
				}
			}
			if got := r.Replace("poll"); got != "poll" {
				t.Fatalf("short secret corrupted poll: %q", got)
			}
		})
	}
}

func TestRedactorSharedVariantRetainsLongSecretSubstringMatching(t *testing.T) {
	for _, variant := range []string{`\u003c\u0026\u003e`, "%3C%26%3E"} {
		for _, reverse := range []bool{false, true} {
			c := &config.Config{}
			c.RouterOS.Password, c.API.Token = "<&>", variant
			if reverse {
				c.RouterOS.Password, c.API.Token = c.API.Token, c.RouterOS.Password
			}
			if got := FromConfig(c).Replace("prefix" + variant + "suffix"); got != "prefix[redacted]suffix" {
				t.Errorf("shared variant %q (reverse=%v) lost long-secret matching: %q", variant, reverse, got)
			}
		}
	}
}
