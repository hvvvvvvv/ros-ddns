package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestOptionalSectionsMayBeEmpty(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"omitted", "{}"},
		{"log only", "log: {level: info}"},
		{"empty", "routeros: {}\ndns: {providers: {}}\nitems: []"},
		{"null", "routeros: null\ndns: null\nitems: null"},
		{"empty credentials", "routeros: {url: '', username: '', password: ''}"},
		{"disabled name only", "items: [{name: paused, disabled: true}]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.raw), "")
			if err != nil {
				t.Fatalf("Parse with empty optional settings: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate with empty optional settings: %v", err)
			}
		})
	}
}

func TestRouterOSWithoutEnabledItems(t *testing.T) {
	for _, tc := range []struct{ name, fields, want string }{
		{"password only", "password: secret", ""},
		{"URL only", "url: https://router.invalid", ""},
		{"invalid URL", "url: 'https://['", "routeros.url: invalid URL"},
		{"scheme", "url: ftp://router.invalid", "routeros.url: unsupported scheme"},
		{"host", "url: https://:443", "routeros.url: host is required"},
		{"userinfo", "url: https://user:secret@router.invalid", "routeros.url: userinfo is not allowed"},
		{"nonnumeric port", "url: https://router.invalid:abc", "routeros.url: invalid URL"},
		{"empty port", "url: 'https://router.invalid:'", "routeros.url: port must be"},
		{"zero port", "url: https://router.invalid:0", "routeros.url: port must be"},
		{"port range", "url: https://router.invalid:65536", "routeros.url: port must be"},
		{"query", "url: 'https://router.invalid?x=y'", "routeros.url: query strings and fragments are not allowed"},
		{"empty query", "url: 'https://router.invalid?'", "routeros.url: query strings and fragments are not allowed"},
		{"fragment", "url: 'https://router.invalid#fragment'", "routeros.url: query strings and fragments are not allowed"},
		{"empty fragment", "url: 'https://router.invalid#'", "routeros.url: query strings and fragments are not allowed"},
		{"rest suffix", "url: https://router.invalid/base/rest/", "routeros.url: do not include /rest"},
		{"zero timeout", "timeout: 0s", "routeros.timeout: must be greater than 0"},
		{"negative timeout", "timeout: -1s", "routeros.timeout: must be greater than 0"},
		{"TLS conflict", "tls: {insecureSkipVerify: true, caFile: test.pem}", "routeros.tls: insecureSkipVerify and caFile are mutually exclusive"},
	} {
		for _, items := range []struct{ name, raw string }{
			{"no items", ""},
			{"disabled only", "items: [{name: paused, disabled: true}]"},
		} {
			t.Run(tc.name+"/"+items.name, func(t *testing.T) {
				raw := "routeros: {" + tc.fields + "}\n" + items.raw
				_, err := Parse([]byte(raw), "")
				if tc.want == "" {
					if err != nil {
						t.Fatalf("Parse without enabled items: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Parse error = %v, want %q", err, tc.want)
				}
				if strings.Contains(err.Error(), "must not be empty") {
					t.Fatalf("unused RouterOS settings required credentials: %v", err)
				}
			})
		}
	}
}

func TestEnabledItemsRequireRouterOS(t *testing.T) {
	const items = `items:
  - name: paused
    disabled: true
  - name: active
    source: {interface: wan, family: ipv4}
    addressList: {list: ddns}
`
	for _, tc := range []struct{ name, routeros, want string }{
		{"missing", "", "routeros.url: must not be empty"},
		{"password only", "routeros: {password: secret}\n", "routeros.url: must not be empty"},
		{"username missing", "routeros: {url: https://router.invalid}\n", "routeros.username: must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.routeros+items), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse with enabled item: error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDisabledItemsSkipOptionalFields(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		want         []string
	}{
		{"name only", "", []string{".source.interface:", ".source.family:", "at least one of domains or addressList is required"}},
		{"invalid substructures", `    source:
      family: other
      eui64-mac: invalid
      select: {scope: other, prefix: invalid, index: -1}
    domains: [{provider: missing, domain: bad_domain, rr: bad*, ttl: 0}]
    addressList: {list: '', timeout: invalid}
`, []string{
			".source.interface:", ".source.family:", ".source.eui64-mac:",
			".source.select.scope:", ".source.select.prefix:", ".source.select.index:",
			".domains[0].provider: unknown provider", ".domains[0].domain:", ".domains[0].rr:", ".domains[0].ttl:",
			".addressList.list:", ".addressList.timeout:",
		}},
	} {
		for _, disabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/disabled=%t", tc.name, disabled), func(t *testing.T) {
				raw := "routeros: {url: https://router.invalid, username: ddns}\n" +
					fmt.Sprintf("items:\n  - name: draft\n    disabled: %t\n", disabled) + tc.fields
				cfg, err := Parse([]byte(raw), "")
				if disabled {
					if err != nil {
						t.Fatalf("Parse disabled item: %v", err)
					}
					if err := cfg.Validate(); err != nil {
						t.Fatalf("Validate disabled item: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatal("Parse accepted an incomplete enabled item")
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Parse error = %v, want %q", err, want)
					}
				}
			})
		}
	}
}

func TestDisabledItemNamesRemainValidated(t *testing.T) {
	for _, name := range []string{"", "Invalid", "has space", "-prefix", strings.Repeat("a", 65)} {
		t.Run(name, func(t *testing.T) {
			raw := fmt.Sprintf("items: [{name: %q, disabled: true}]", name)
			_, err := Parse([]byte(raw), "")
			if err == nil || !strings.Contains(err.Error(), ".name:") || strings.Count(err.Error(), "\n - ") != 1 {
				t.Fatalf("Parse error = %v, want only an item name error", err)
			}
		})
	}
}

func TestItemNamesUniqueAcrossDisabledStates(t *testing.T) {
	for _, firstDisabled := range []bool{false, true} {
		for _, secondDisabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%t/%t", firstDisabled, secondDisabled), func(t *testing.T) {
				raw := fmt.Sprintf(`routeros: {url: https://router.invalid, username: ddns}
items:
  - name: same
    disabled: %t
    source: {interface: wan, family: ipv4}
    addressList: {list: first}
  - name: same
    disabled: %t
    source: {interface: wan, family: ipv4}
    addressList: {list: second}
`, firstDisabled, secondDisabled)
				_, err := Parse([]byte(raw), "")
				const want = "invalid config:\n - items[1] (same).name: duplicate item name \"same\""
				if err == nil || err.Error() != want {
					t.Fatalf("Parse error = %v, want %s", err, want)
				}
			})
		}
	}
}

func TestProvidersRemainValidatedWithoutEnabledItems(t *testing.T) {
	for _, tc := range []struct{ name, fields, want string }{
		{"credentials omitted", "type: aliyun", ""},
		{"empty credentials", "type: aliyun, accessKeyId: '', accessKeySecret: ''", ""},
		{"unsupported type", "type: other", "dns.providers.test.type:"},
		{"zero timeout", "type: aliyun, timeout: 0s", "dns.providers.test.timeout:"},
		{"zero QPS", "type: aliyun, qps: 0", "dns.providers.test.qps:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "dns: {providers: {test: {" + tc.fields + "}}}\nitems: [{name: paused, disabled: true}]"
			_, err := Parse([]byte(raw), "")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Parse with optional provider credentials: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAPIListenPortValidation(t *testing.T) {
	for _, port := range []string{"", "-1", "0", "65536", "999999999999999999999", "http", "+80", " 80", "80 ", "80\t", "8 0", "80\n", "８０", "0x50", "80.0"} {
		t.Run("invalid/"+port, func(t *testing.T) {
			input := fmt.Sprintf("api:\n  listen: %q\n", "127.0.0.1:"+port) + minimalConfig
			if _, err := loadString(t, input); err == nil || !strings.Contains(err.Error(), "api.listen") {
				t.Fatalf("Load error = %v, want invalid API port", err)
			}
		})
	}
	for _, address := range []string{"127.0.0.1:1", "127.0.0.1:65535", "127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		t.Run("valid/"+address, func(t *testing.T) {
			cfg, err := loadString(t, fmt.Sprintf("api:\n  listen: %q\n", address)+minimalConfig)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.API.Listen != address {
				t.Errorf("listen = %q, want %q", cfg.API.Listen, address)
			}
		})
	}
}

func TestRouterOSURLQueryAndFragmentRejected(t *testing.T) {
	for _, tc := range []struct{ name, suffix string }{
		{"query", "?token=sensitive-token"},
		{"fragment", "#sensitive-token"},
		{"force query", "?"},
		{"empty fragment", "#"},
		{"both", "?token=sensitive-token#other"},
		{"base path query", "/base?token=sensitive-token"},
		{"base path fragment", "/base#sensitive-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "https://router.invalid" + tc.suffix
			input := strings.Replace(minimalConfig, "https://192.168.88.1", raw, 1)
			_, err := loadString(t, input)
			if err == nil || !strings.Contains(err.Error(), "routeros.url") {
				t.Fatalf("Load error = %v, want URL validation error", err)
			}
			if strings.Contains(err.Error(), "sensitive-token") || strings.Contains(err.Error(), raw) {
				t.Errorf("validation error leaks URL: %v", err)
			}
		})
	}
}

func TestRouterOSBaseURLsRemainValid(t *testing.T) {
	for _, raw := range []string{
		"http://router.invalid",
		"https://router.invalid:1",
		"https://router.invalid:65535",
		"https://router.invalid:8443",
		"https://router.invalid:0080/base/path",
		"https://router.invalid/base/path",
		"https://router.invalid/base%3Fpath%23part",
		"http://[::1]",
		"https://[2001:db8::1]/base/path",
		"https://[2001:db8::1]:1",
		"https://[2001:db8::1]:65535/base/path",
		"https://[fe80::1%25eth0]:8443/base/path",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg, err := loadString(t, strings.Replace(minimalConfig, "https://192.168.88.1", raw, 1))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RouterOS.URL != raw || redactURL(raw) != raw {
				t.Errorf("base URL changed: config=%q summary=%q", cfg.RouterOS.URL, redactURL(raw))
			}
		})
	}
}

func TestRouterOSURLAuthorityValidation(t *testing.T) {
	for i, raw := range []string{
		"http://:80",
		"https://:443/base",
		"https://[]:80",
		"https://router.invalid:0",
		"https://router.invalid:65536",
		"https://router.invalid:999999999999999999999",
		"https://router.invalid:-1",
		"https://router.invalid:sensitive-port",
		"https://router.invalid:",
		"https://router.invalid:/base",
		"https://router.invalid:+80",
		"https://router.invalid:80.0",
		"https://router.invalid:0x50",
		"https://router.invalid:8 0",
		"https://router.invalid:８０",
		"https://router.invalid:%38%30",
		"https://[2001:db8::1]:0",
		"https://[2001:db8::1]:65536",
		"https://[2001:db8::1]:-1",
		"https://[2001:db8::1]:sensitive-port",
		"https://[2001:db8::1]:",
		"https://[2001:db8::1]:/base",
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			input := strings.Replace(minimalConfig, "https://192.168.88.1", fmt.Sprintf("%q", raw), 1)
			_, err := loadString(t, input)
			if err == nil || !strings.Contains(err.Error(), "routeros.url:") {
				t.Fatalf("Load URL %q: error = %v, want invalid RouterOS authority", raw, err)
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "sensitive-port") {
				t.Errorf("validation error leaks URL or port: %v", err)
			}
		})
	}
}

func TestRouterOSURLRestrictionsRemainValid(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"scheme", "ftp://router.invalid:21", "unsupported scheme"},
		{"userinfo", "https://admin:sensitive-password@router.invalid:443", "userinfo"},
		{"userinfo and port", "https://admin:sensitive-password@router.invalid:65536", "userinfo"},
		{"rest", "https://router.invalid:443/base/rest/", "do not include /rest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(minimalConfig, "https://192.168.88.1", tc.raw, 1)
			_, err := loadString(t, input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sensitive-password") {
				t.Errorf("validation error leaks password: %v", err)
			}
		})
	}
}

func TestAddressListTimeoutSemanticValidation(t *testing.T) {
	for _, value := range []string{
		"1h30m", "1w2d3h4m5s", "00:10:00", "10:00", "99:00:00",
		"35w3d13h13m56s", "21474836s", "35w3d13h13m55s", "248d",
		"00:00.5", "00:59:59.999999999", "59:59", "600", "1.5",
		"", "0", "0s", "00:00", "00:00:00", "0w0d0h0m0s", " 1h30m ", "  ",
	} {
		t.Run("valid/"+value, func(t *testing.T) {
			input := strings.Replace(minimalConfig, "list: ddns-wan-v6", "list: ddns-wan-v6\n      timeout: "+fmt.Sprintf("%q", value), 1)
			cfg, err := loadString(t, input)
			if err != nil {
				t.Fatalf("Load valid timeout: %v", err)
			}
			if got, want := cfg.Items[0].AddressList.Timeout, strings.TrimSpace(value); got != want {
				t.Errorf("timeout = %q, want %q", got, want)
			}
		})
	}
	for i, tc := range []struct{ value, want string }{
		{"36w", "must not exceed"},
		{"35w3d13h13m57s", "must not exceed"},
		{"21474837s", "must not exceed"},
		{"21474836.1", "must not exceed"},
		{"999999999999999999999w", "must not exceed"},
		{"999999999999999999999", "must not exceed"},
		{"999999999999999999999:00:00", "must not exceed"},
		{"01:60", "minutes and seconds"},
		{"00:00:60", "minutes and seconds"},
		{"00:60:00", "minutes and seconds"},
		{"60:00", "minutes and seconds"},
		{"01::00", "invalid RouterOS duration"},
		{"1:2:3:4", "invalid RouterOS duration"},
		{"1h30", "invalid RouterOS duration"},
		{"1w2x", "invalid RouterOS duration"},
		{"h", "invalid RouterOS duration"},
		{"1ms", "invalid RouterOS duration"},
		{"-1s", "invalid RouterOS duration"},
		{"1h 30m", "invalid RouterOS duration"},
		{"sensitive-timeout", "invalid RouterOS duration"},
	} {
		t.Run(fmt.Sprintf("invalid/%d", i), func(t *testing.T) {
			input := strings.Replace(minimalConfig, "list: ddns-wan-v6", "list: ddns-wan-v6\n      timeout: "+fmt.Sprintf("%q", tc.value), 1)
			_, err := loadString(t, input)
			if err == nil || !strings.Contains(err.Error(), "addressList.timeout:") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load timeout %q: error = %v, want timeout error containing %q", tc.value, err, tc.want)
			}
			if strings.Contains(err.Error(), "sensitive-timeout") {
				t.Errorf("validation error leaks timeout: %v", err)
			}
		})
	}
}
