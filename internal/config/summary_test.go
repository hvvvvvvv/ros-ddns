package config

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"valid without userinfo", "https://router:8443/path", "https://router:8443/path"},
		{"query and fragment", "https://router:8443/path?query=value#fragment", "https://router:8443/path"},
		{"force query", "https://router?", "https://router"},
		{"empty fragment", "https://router#", "https://router"},
		{"valid password", "https://admin:secret@router:8443/path", "https://router:8443/path"},
		{"valid username only", "https://admin@router/path", "https://router/path"},
		{"invalid port", "https://admin:secret@router:bad/path", "[invalid URL]"},
		{"invalid IPv6 host", "https://admin:secret@[2001:db8::1", "[invalid URL]"},
		{"invalid password escape", "https://admin:sec%zzret@router", "[invalid URL]"},
		{"invalid path escape", "https://admin:secret@router/%zz", "[invalid URL]"},
		{"multiple at signs", "https://admin:sec@ret@router:bad", "[invalid URL]"},
		{"username only with invalid port", "https://admin@router:bad", "[invalid URL]"},
		{"custom scheme", "git+ssh://admin:secret@router:bad", "[invalid URL]"},
		{"invalid without userinfo", "https://router:bad", "[invalid URL]"},
		{"at sign in path", "https://router/admin@host", "https://router/admin@host"},
		{"at sign in query", "https://router:bad?email=admin@host", "[invalid URL]"},
		{"at sign in fragment", "https://router:bad#admin@host", "[invalid URL]"},
		{"arbitrary text", "contact admin:secret@router:bad", "[invalid URL]"},
		{"embedded URL in text", "see https://admin:secret@router:bad", "[invalid URL]"},
		{"invalid scheme", "1https://admin:secret@router:bad", "[invalid URL]"},
		{"scheme relative", "//admin:secret@router:bad/path", "[invalid URL]"},
		{"leading whitespace", " https://admin:secret@router:bad/path ", "[invalid URL]"},
		{"missing authority", "https:admin:secret@router", "[invalid URL]"},
		{"missing slash", "https:/admin:secret@router", "[invalid URL]"},
		{"missing slash and invalid escape", "https:/admin:secret@router/%zz", "[invalid URL]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactURL(tc.raw); got != tc.want {
				t.Errorf("redactURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSummaryUnsetRouterOS(t *testing.T) {
	cfg := Default()
	summary := Summary(cfg)
	if !strings.Contains(summary, "routeros: (not configured)") || strings.Contains(summary, "[invalid URL]") {
		t.Fatalf("summary renders unset RouterOS as invalid: %q", summary)
	}
	cfg.RouterOS.URL = "https://router.invalid"
	if summary := Summary(cfg); !strings.Contains(summary, "routeros: https://router.invalid") {
		t.Fatalf("summary changed configured RouterOS output: %q", summary)
	}
}

func TestSummaryStripsURLQueryAndFragment(t *testing.T) {
	for _, base := range []string{
		"https://router.invalid", "https://router.invalid/base", "https://router.invalid:bad/base",
		"https://router.invalid/%zz", "https://admin:userinfo-secret@router.invalid:bad/base",
		"//admin:userinfo-secret@router.invalid:bad/base", "1https://admin:userinfo-secret@router.invalid:bad/base",
		"https:admin:userinfo-secret@router.invalid", "https:/admin:userinfo-secret@router.invalid",
		"https:/admin:userinfo-secret@router.invalid/%zz",
	} {
		for _, suffix := range []string{"?token=query-secret", "#fragment-secret", "?token=query-secret#fragment-secret"} {
			t.Run(base+suffix, func(t *testing.T) {
				cfg := Default()
				cfg.RouterOS.URL = base + suffix
				for name, output := range map[string]string{"redactURL": redactURL(cfg.RouterOS.URL), "Summary": Summary(cfg)} {
					for _, secret := range []string{"query-secret", "fragment-secret", "userinfo-secret"} {
						if strings.Contains(output, secret) {
							t.Errorf("%s leaks URL credentials: %q", name, output)
						}
					}
				}
			})
		}
	}
}

func TestRouterOSURLSecretsRedacted(t *testing.T) {
	const username = "sensitive-admin"
	const password = "sensitive-password"
	for _, tc := range []struct {
		name   string
		suffix string
	}{
		{"invalid port", "@router:bad"},
		{"invalid IPv6 host", "@[2001:db8::1"},
		{"invalid password escape", "%zz@router"},
		{"invalid path escape", "@router/%zz"},
		{"valid userinfo", "@router"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "https://" + username + ":" + password + tc.suffix
			input := strings.Replace(minimalConfig, "https://192.168.88.1", raw, 1)
			_, err := loadString(t, input)
			if err == nil || !strings.Contains(err.Error(), "routeros.url") {
				t.Fatalf("Load error = %v, want routeros.url validation error", err)
			}
			cfg := Default()
			cfg.RouterOS.URL = raw
			for name, output := range map[string]string{
				"error": err.Error(), "summary": Summary(cfg), "redactURL": redactURL(raw),
			} {
				for _, secret := range []string{username, password} {
					if strings.Contains(output, secret) {
						t.Errorf("%s leaks URL userinfo: %q", name, output)
					}
				}
			}
		})
	}
}
