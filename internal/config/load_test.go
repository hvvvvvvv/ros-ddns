package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestEnvScalarValues(t *testing.T) {
	for _, value := range []string{
		"*secret*", "null", "~", "true", "123", "a#b", "a: b", "# comment",
		"  leading and trailing  ", "line one\nline two\n", "\tquoted \"double\" and 'single'\r\n",
		"\napi:\n  token: injected\n---\n", "", "${NOT_EXPANDED_AGAIN}",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("ROS_DDNS_TEST_PASSWORD", value)
			input := strings.Replace(minimalConfig, "password: super-secret", "password: ${ROS_DDNS_TEST_PASSWORD}", 1)
			cfg, err := loadString(t, input)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RouterOS.Password != value {
				t.Errorf("password = %q, want %q", cfg.RouterOS.Password, value)
			}
		})
	}
}

func TestEnvNullTokenRemainsSet(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_TOKEN", "null")
	cfg, err := loadString(t, "api:\n  listen: 0.0.0.0:8080\n  token: ${ROS_DDNS_TEST_TOKEN}\n"+minimalConfig)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Token != "null" {
		t.Errorf("token = %q, want literal null", cfg.API.Token)
	}
	if !strings.Contains(Summary(cfg), "token=set") {
		t.Error("summary does not report a configured token")
	}
}

func TestEnvCommentsAreIgnored(t *testing.T) {
	const missing = "ROS_DDNS_TEST_COMMENT_MISSING"
	t.Setenv(missing, "temporary")
	if err := os.Unsetenv(missing); err != nil {
		t.Fatal(err)
	}
	input := "# ${" + missing + "}\n" + strings.Replace(minimalConfig, "password: super-secret", "password: super-secret # ${"+missing+"}", 1)
	input += "\n# ${1INVALID}\n# ${unterminated\n"
	if _, err := loadString(t, input); err != nil {
		t.Fatalf("Load expanded a comment: %v", err)
	}
}

func TestEnvAliasesAndMergesExpandOnce(t *testing.T) {
	input := strings.Replace(minimalConfig, "password: super-secret", "password: &secret $${ROS_DDNS_TEST_LITERAL}", 1)
	input += "\napi:\n  <<: &api-defaults\n    token: *secret\n  listen: 0.0.0.0:8080\n"
	cfg, err := loadString(t, input)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, value := range map[string]string{"password": cfg.RouterOS.Password, "token": cfg.API.Token} {
		if value != "${ROS_DDNS_TEST_LITERAL}" {
			t.Errorf("%s = %q, want unexpanded literal reference", name, value)
		}
	}

	t.Setenv("ROS_DDNS_TEST_TIMEOUT", "250ms")
	input = strings.Replace(providerConfig, "routeros:", "routeros:\n  <<: &timeouts {timeout: '${ROS_DDNS_TEST_TIMEOUT}'}", 1)
	input = strings.Replace(input, "type: aliyun", "type: aliyun\n      <<: *timeouts", 1)
	cfg, err = loadString(t, input)
	if err != nil {
		t.Fatalf("Load merged alias: %v", err)
	}
	if cfg.RouterOS.Timeout.Duration != 250*time.Millisecond || cfg.DNS.Providers["ali-main"].Timeout.Duration != 250*time.Millisecond {
		t.Error("environment value in merged anchor was not applied to both configurations")
	}
}

func TestEnvExpansionPreservesStrictYAML(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_PASSWORD", "secret-value")
	input := strings.Replace(minimalConfig, "password: super-secret", "password: ${ROS_DDNS_TEST_PASSWORD}", 1)
	for name, contents := range map[string]string{
		"unknown field":      "unknown: true\n" + input,
		"merged unknown":     "api:\n  <<: &defaults {unknown: true}\n" + input,
		"aliased unknown":    "api: &defaults {unknown: true}\nlog: {<<: *defaults}\n" + input,
		"multiple documents": input + "\n---\napi: {}\n",
		"empty trailing doc": input + "\n---\n",
		"recursive alias":    "api: &recursive {<<: *recursive}\n" + input,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadString(t, contents); err == nil {
				t.Fatal("Load accepted invalid YAML structure")
			}
		})
	}
}

func TestEnvExpandedNumbersAndBooleansStayStrings(t *testing.T) {
	for _, tc := range []struct{ name, value, input string }{
		{"boolean", "true", "runtime:\n  syncOnStart: ${ROS_DDNS_TEST_TYPED}\n" + minimalConfig},
		{"legacy boolean off", "off", "runtime:\n  syncOnStart: ${ROS_DDNS_TEST_TYPED}\n" + minimalConfig},
		{"legacy boolean YES", "YES", "runtime:\n  syncOnStart: ${ROS_DDNS_TEST_TYPED}\n" + minimalConfig},
		{"TLS boolean", "on", strings.Replace(minimalConfig, "routeros:", "routeros:\n  tls:\n    insecureSkipVerify: ${ROS_DDNS_TEST_TYPED}", 1)},
		{"item boolean", "no", strings.Replace(minimalConfig, "name: wan-v6", "name: wan-v6\n    disabled: ${ROS_DDNS_TEST_TYPED}", 1)},
		{"aliased boolean", "off", strings.Replace(minimalConfig, "password: super-secret", "password: &flag ${ROS_DDNS_TEST_TYPED}", 1) + "\nruntime: {syncOnStart: *flag}\n"},
		{"merged boolean", "off", "runtime:\n  <<: {syncOnStart: '${ROS_DDNS_TEST_TYPED}'}\n" + minimalConfig},
		{"integer", "5", strings.Replace(providerConfig, "type: aliyun", "type: aliyun\n      qps: ${ROS_DDNS_TEST_TYPED}", 1)},
		{"tagged integer", "5", strings.Replace(providerConfig, "type: aliyun", "type: aliyun\n      qps: !!int ${ROS_DDNS_TEST_TYPED}", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROS_DDNS_TEST_TYPED", tc.value)
			_, err := loadString(t, tc.input)
			if err == nil {
				t.Fatal("Load coerced an environment-expanded string into a numeric or boolean field")
			}
			if !strings.Contains(err.Error(), "environment-expanded scalars are strings") {
				t.Errorf("error omits guidance on expanded types: %v", err)
			}
		})
	}
}

func TestEnvBooleanMergeOverride(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_FLAG", "off")
	for _, runtime := range []string{
		"runtime:\n  <<: {syncOnStart: '${ROS_DDNS_TEST_FLAG}'}\n  syncOnStart: true\n",
		"runtime:\n  <<: [{syncOnStart: true}, {syncOnStart: '${ROS_DDNS_TEST_FLAG}'}]\n",
	} {
		cfg, err := loadString(t, runtime+minimalConfig)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.Runtime.SyncOnStart {
			t.Error("literal boolean did not override merged environment value")
		}
	}
}

func TestEnvBooleanLiteralsRemainUnchanged(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_PASSWORD", "secret-value")
	input := strings.Replace(minimalConfig, "super-secret", "${ROS_DDNS_TEST_PASSWORD}", 1)
	for value, want := range map[string]bool{"true": true, "false": false, "on": true, "off": false, "YES": true, "NO": false} {
		t.Run(value, func(t *testing.T) {
			cfg, err := loadString(t, "runtime:\n  syncOnStart: "+value+"\n"+input)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Runtime.SyncOnStart != want {
				t.Errorf("syncOnStart = %t, want %t", cfg.Runtime.SyncOnStart, want)
			}
		})
	}
}

func TestEnvErrorsDoNotLeakValues(t *testing.T) {
	const secret = "sensitive-expanded-secret"
	const missing = "ROS_DDNS_TEST_ERROR_MISSING"
	t.Setenv("ROS_DDNS_TEST_SECRET", secret)
	t.Setenv(missing, "temporary")
	if err := os.Unsetenv(missing); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{
		"raw syntax":       "routeros: [\n  password: ${ROS_DDNS_TEST_SECRET}\n",
		"missing variable": strings.Replace(minimalConfig, "super-secret", "${ROS_DDNS_TEST_SECRET}${"+missing+"}", 1),
		"invalid name":     strings.Replace(minimalConfig, "super-secret", "${ROS_DDNS_TEST_SECRET}${1INVALID}", 1),
		"unterminated":     strings.Replace(minimalConfig, "super-secret", "${ROS_DDNS_TEST_SECRET}${unterminated", 1),
		"type error":       "runtime:\n  syncOnStart: ${ROS_DDNS_TEST_SECRET}\n" + minimalConfig,
		"duration error":   "runtime:\n  pollInterval: ${ROS_DDNS_TEST_SECRET}\n" + minimalConfig,
		"expanded key":     "${ROS_DDNS_TEST_SECRET}: true\n" + minimalConfig,
		"semantic error":   "log:\n  level: ${ROS_DDNS_TEST_SECRET}\n" + minimalConfig,
		"aliased semantic error": strings.Replace(minimalConfig, "password: super-secret", "password: &secret ${ROS_DDNS_TEST_SECRET}", 1) +
			"\nlog:\n  level: *secret\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadString(t, input)
			if err == nil {
				t.Fatal("Load accepted invalid config")
			}
			if strings.Contains(err.Error(), secret[:7]) {
				t.Errorf("error leaks an expanded value: %v", err)
			}
			if name == "missing variable" && !strings.Contains(err.Error(), missing) {
				t.Errorf("error omits the missing variable name: %v", err)
			}
		})
	}
}

func TestEnvErrorsPreserveFieldDetails(t *testing.T) {
	for _, secrets := range []struct{ name, password, id, key string }{
		{"short", "p", "ak", "sk"},
		{"long", "private-password-value", "private-access-key-id", "private-access-key-secret"},
	} {
		t.Run(secrets.name, func(t *testing.T) {
			t.Setenv("ROS_DDNS_TEST_PASSWORD", secrets.password)
			t.Setenv("ROS_DDNS_TEST_ID", secrets.id)
			t.Setenv("ROS_DDNS_TEST_KEY", secrets.key)
			input := strings.NewReplacer(
				"super-secret", "${ROS_DDNS_TEST_PASSWORD}",
				"ak-id", "${ROS_DDNS_TEST_ID}",
				"ak-secret", "${ROS_DDNS_TEST_KEY}",
			).Replace(providerConfig)
			if _, err := Parse([]byte(input), "test.yaml"); err != nil {
				t.Fatalf("valid environment credentials: %v", err)
			}
			for _, tc := range []struct{ name, input, want string }{
				{"ttl", strings.Replace(input, "rr: Home", "rr: Home\n        ttl: 0", 1),
					"invalid config:\n - items[0] (wan-v4).domains[0].ttl: must be between 1 and 86400"},
				{"log level", "log:\n  level: verbose\n" + input,
					"invalid config:\n - log.level: unsupported value \"verbose\" (debug|info|warn|error)"},
				{"unknown field", "log:\n  levle: info\n" + input,
					"parse YAML: yaml: unmarshal errors:\n  line 2: field levle not found in type config.LogConfig; environment-expanded scalars are strings (use YAML literals for numbers and booleans)"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := Parse([]byte(tc.input), "test.yaml")
					if err == nil || err.Error() != tc.want {
						t.Fatalf("Parse error = %v, want %q", err, tc.want)
					}
					t.Log(err)
				})
			}
		})
	}
}

func TestEnvShortSecretOnlyRedactsTokens(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_PASSWORD", "p")
	input := strings.Replace(minimalConfig, "super-secret", "${ROS_DDNS_TEST_PASSWORD}", 1)
	input = strings.Replace(input, "https://", "ip://", 1)
	_, err := Parse([]byte("log:\n  level: p\n"+input), "test.yaml")
	const want = "invalid config:\n - log.level: unsupported value \"[redacted]\" (debug|info|warn|error)\n - routeros.url: unsupported scheme \"ip\" (http|https)"
	if err == nil || err.Error() != want {
		t.Fatalf("Parse error = %v, want %q", err, want)
	}

	_, err = Parse([]byte("p: true\nparse: true\nip: true\nhttp: true\n"+input), "test.yaml")
	if err == nil {
		t.Fatal("Parse accepted unknown fields")
	}
	for _, field := range []string{"[redacted]", "parse", "ip", "http"} {
		if !strings.Contains(err.Error(), "field "+field+" not found in type config.Config") {
			t.Errorf("missing or corrupted field %q: %v", field, err)
		}
	}
	if strings.Contains(err.Error(), "field p not found") {
		t.Errorf("standalone secret not redacted: %v", err)
	}
}

func TestEnvErrorsRedactRepresentations(t *testing.T) {
	const ref = "${ROS_DDNS_TEST_SECRET}"
	for _, tc := range []struct{ name, value, input, want string }{
		{"quoted log", "  LeakProbe \"quoted\"\nValue  ", "log:\n  level: " + ref + "\n" + minimalConfig,
			`log.level: unsupported value "[redacted]"`},
		{"short normalized log", "  P  ", "log:\n  level: " + ref + "\n" + minimalConfig,
			`log.level: unsupported value "[redacted]"`},
		{"aliased log", "  LeakProbe \"quoted\"\nValue  ", strings.Replace(minimalConfig, "password: super-secret", "password: &secret "+ref, 1) + "\nlog: {level: *secret}\n",
			`log.level: unsupported value "[redacted]"`},
		{"trimmed provider", "  LeakProbe  ", strings.Replace(providerConfig, "provider: ali-main", "provider: "+ref, 1),
			`provider: unknown provider "[redacted]"`},
		{"normalized domain", "  LeakProbe_Invalid. \n", strings.Replace(providerConfig, "domain: Example.COM.", "domain: "+ref, 1),
			`domain: invalid domain "[redacted]"`},
		{"raw item label", " LeakProbe\nName ", strings.Replace(minimalConfig, "name: wan-v6", "name: "+ref, 1),
			`items[0] ([redacted]).name: "[redacted]" must match`},
		{"short boolean", "true", "runtime:\n  syncOnStart: " + ref + "\n" + minimalConfig,
			"cannot unmarshal !!str `[redacted]` into bool"},
		{"boolean preview", "LeakProbe-LongSecret", "runtime:\n  syncOnStart: " + ref + "\n" + minimalConfig,
			"cannot unmarshal !!str `[redacted]` into bool"},
		{"UTF-8 preview", "秘密密码LeakProbe", "runtime:\n  syncOnStart: " + ref + "\n" + minimalConfig,
			"cannot unmarshal !!str `[redacted]` into bool"},
		{"numeric string", "42", strings.Replace(providerConfig, "rr: Home", "rr: Home\n        ttl: "+ref, 1),
			"cannot unmarshal !!str `[redacted]` into int"},
		{"numeric preview", "LeakProbe-LongSecret", strings.Replace(providerConfig, "rr: Home", "rr: Home\n        ttl: "+ref, 1),
			"cannot unmarshal !!str `[redacted]` into int"},
		{"expanded key", "LeakProbe\n\"Key\"", ref + ": true\n" + minimalConfig,
			"field [redacted] not found"},
		{"same spelling", ref, "log:\n  level: " + ref + "\n" + minimalConfig,
			`log.level: unsupported value "[redacted]"`},
		{"duplicate key", "username", strings.Replace(minimalConfig, "password: super-secret", ref+": LeakProbe", 1),
			`mapping key "[redacted]" already defined`},
		{"duration escapes", " LeakProbe\n\"value\" ", "runtime:\n  pollInterval: " + ref + "\n" + minimalConfig,
			`invalid duration "[redacted]"`},
		{"duration unit", "123LeakProbeUnit", "runtime:\n  pollInterval: " + ref + "\n" + minimalConfig,
			`unknown unit "[redacted]" in duration "[redacted]"`},
		{"CIDR fragment", "LeakProbe/64", strings.Replace(minimalConfig, "family: ipv6", "family: ipv6\n      select:\n        prefix: "+ref, 1),
			`ParseAddr("[redacted]")`},
		{"URL scheme", "LeakProbe://host", strings.Replace(minimalConfig, "https://192.168.88.1", ref, 1),
			`routeros.url: unsupported scheme "[redacted]"`},
		{"canonical CIDR", "2001:0DB8:0000::1/64", strings.Replace(providerConfig, "family: ipv4", "family: ipv4\n      select:\n        prefix: "+ref, 1),
			"source.select.prefix: [redacted] is not an IPv4 prefix"},
		{"MAC formatting", "LeakProbe123", strings.Replace(minimalConfig, "family: ipv6", "family: ipv6\n      eui64-mac: "+ref, 1),
			`address [redacted]: invalid MAC address`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROS_DDNS_TEST_SECRET", tc.value)
			_, err := Parse([]byte(tc.input), "test.yaml")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want %q", err, tc.want)
			}
			if strings.Contains(strings.ToLower(err.Error()), "leakprobe") {
				t.Errorf("error includes secret material: %v", err)
			}
			t.Log(err)
		})
	}
}

func TestTimeoutDefaultsAndValidation(t *testing.T) {
	for _, field := range []struct {
		name   string
		anchor string
		prefix string
		value  func(*Config) Duration
	}{
		{"routeros.timeout", "  username: ddns", "  timeout: ", func(c *Config) Duration { return c.RouterOS.Timeout }},
		{"dns.providers.ali-main.timeout", "      type: aliyun", "      timeout: ", func(c *Config) Duration { return c.DNS.Providers["ali-main"].Timeout }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				value string
				want  time.Duration
			}{
				{"absent", "", 10 * time.Second},
				{"null", "null", 10 * time.Second},
				{"zero", "0s", 0},
				{"negative", "-1s", -time.Second},
				{"ordinary", "250ms", 250 * time.Millisecond},
			} {
				t.Run(tc.name, func(t *testing.T) {
					input := providerConfig
					if tc.value != "" {
						input = strings.Replace(input, field.anchor, field.anchor+"\n"+field.prefix+tc.value, 1)
					}
					cfg, err := loadString(t, input)
					if tc.want <= 0 {
						if err == nil || !strings.Contains(err.Error(), field.name+": must be greater than 0") {
							t.Fatalf("Load error = %v, want %s validation error", err, field.name)
						}
						return
					}
					if err != nil {
						t.Fatalf("Load: %v", err)
					}
					cfg.Normalize()
					if got := field.value(cfg).Duration; got != tc.want {
						t.Errorf("timeout = %s, want %s", got, tc.want)
					}
					if got, want := field.value(cfg).set, tc.value != "" && tc.value != "null"; got != want {
						t.Errorf("timeout presence = %t, want %t", got, want)
					}
				})
			}
		})
	}
}

func TestDurationYAMLPresence(t *testing.T) {
	defaults := Default()
	for _, duration := range []Duration{
		defaults.RouterOS.Timeout, defaults.Runtime.PollInterval,
		defaults.Runtime.StableFor, defaults.Runtime.ReconcileTimeout,
		defaults.Runtime.VerifyInterval, defaults.Runtime.MaxBackoff,
	} {
		if duration.set {
			t.Errorf("default duration %s marked as explicitly set", duration.Duration)
		}
	}
	for _, tc := range []struct {
		input string
		want  time.Duration
		set   bool
	}{
		{"{}", 10 * time.Second, false},
		{"timeout: null", 10 * time.Second, false},
		{"timeout: 0s", 0, true},
		{"timeout: -1s", -time.Second, true},
		{"timeout: 250ms", 250 * time.Millisecond, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got := defaults.RouterOS
			if err := yaml.Unmarshal([]byte(tc.input), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Timeout.Duration != tc.want || got.Timeout.set != tc.set {
				t.Errorf("timeout = %s, set = %t; want %s, set = %t", got.Timeout.Duration, got.Timeout.set, tc.want, tc.set)
			}
		})
	}
}

func TestNormalizeProgrammaticValues(t *testing.T) {
	for _, tc := range []struct {
		name        string
		timeout     time.Duration
		qps, ttl    int
		wantTimeout time.Duration
		wantQPS     int
		wantTTL     int
	}{
		{"defaults", 0, 0, 0, 10 * time.Second, 5, 600},
		{"ordinary", 2 * time.Second, 7, 300, 2 * time.Second, 7, 300},
		{"negative", -time.Second, -1, -1, -time.Second, -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.RouterOS.Timeout = Duration{Duration: tc.timeout}
			cfg.DNS.Providers["test"] = ProviderConfig{Timeout: Duration{Duration: tc.timeout}, QPS: tc.qps}
			cfg.Items = []ItemConfig{{Domains: []DomainConfig{{TTL: tc.ttl}}}}
			cfg.Normalize()
			if got := cfg.RouterOS.Timeout.Duration; got != tc.wantTimeout {
				t.Errorf("routeros timeout = %s, want %s", got, tc.wantTimeout)
			}
			provider := cfg.DNS.Providers["test"]
			if provider.Timeout.Duration != tc.wantTimeout || provider.QPS != tc.wantQPS {
				t.Errorf("provider timeout = %s, qps = %d; want %s, %d", provider.Timeout.Duration, provider.QPS, tc.wantTimeout, tc.wantQPS)
			}
			if got := cfg.Items[0].Domains[0].TTL; got != tc.wantTTL {
				t.Errorf("ttl = %d, want %d", got, tc.wantTTL)
			}
		})
	}
}

func TestIntegerDefaultsAndValidation(t *testing.T) {
	for _, field := range []struct {
		name       string
		anchor     string
		prefix     string
		defaultVal int
		wantErr    string
		value      func(*Config) int
	}{
		{"qps", "      type: aliyun", "      qps: ", 5, "dns.providers.ali-main.qps: must be at least 1", func(c *Config) int { return c.DNS.Providers["ali-main"].QPS }},
		{"ttl", "        rr: Home", "        ttl: ", 600, ".ttl: must be between 1 and 86400", func(c *Config) int { return c.Items[0].Domains[0].TTL }},
	} {
		t.Run(field.name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				value string
				want  int
			}{
				{"absent", "", field.defaultVal},
				{"null", "null", field.defaultVal},
				{"zero", "0", 0},
				{"negative", "-1", -1},
				{"ordinary", "42", 42},
			} {
				t.Run(tc.name, func(t *testing.T) {
					input := providerConfig
					if tc.value != "" {
						input = strings.Replace(input, field.anchor, field.anchor+"\n"+field.prefix+tc.value, 1)
					}
					cfg, err := loadString(t, input)
					if tc.want <= 0 {
						if err == nil || !strings.Contains(err.Error(), field.wantErr) {
							t.Fatalf("Load error = %v, want %s validation error", err, field.name)
						}
						return
					}
					if err != nil {
						t.Fatalf("Load: %v", err)
					}
					cfg.Normalize()
					if got := field.value(cfg); got != tc.want {
						t.Errorf("%s = %d, want %d", field.name, got, tc.want)
					}
				})
			}
		})
	}
}

func TestNestedUnknownFieldsRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"log", "log:\n  unknown: true\n" + providerConfig},
		{"runtime", "runtime:\n  unknown: true\n" + providerConfig},
		{"state", "state:\n  unknown: true\n" + providerConfig},
		{"api", "api:\n  unknown: true\n" + providerConfig},
		{"routeros", strings.Replace(providerConfig, "routeros:", "routeros:\n  unknown: true", 1)},
		{"tls", strings.Replace(providerConfig, "routeros:", "routeros:\n  tls:\n    unknown: true", 1)},
		{"dns", strings.Replace(providerConfig, "dns:", "dns:\n  unknown: true", 1)},
		{"provider", strings.Replace(providerConfig, "type: aliyun", "type: aliyun\n      unknown: true", 1)},
		{"item", strings.Replace(providerConfig, "name: wan-v4", "name: wan-v4\n    unknown: true", 1)},
		{"source", strings.Replace(providerConfig, "source:", "source:\n      unknown: true", 1)},
		{"select", strings.Replace(providerConfig, "source:", "source:\n      select:\n        unknown: true", 1)},
		{"domain", strings.Replace(providerConfig, "rr: Home", "rr: Home\n        unknown: true", 1)},
		{"addressList", strings.Replace(minimalConfig, "addressList:", "addressList:\n      unknown: true", 1)},
		{"provider merge", strings.Replace(providerConfig, "type: aliyun", "type: aliyun\n      <<: {unknown: true}", 1)},
		{"domain merge", strings.Replace(providerConfig, "rr: Home", "rr: Home\n        <<: {unknown: true}", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadString(t, tc.input)
			if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
				t.Fatalf("Load error = %v, want unknown field rejection", err)
			}
		})
	}
}

func TestMergedExplicitZeroRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		anchor  string
		merge   string
		wantErr string
	}{
		{"routeros timeout", "  username: ddns", "  <<: {timeout: 0s}", "routeros.timeout: must be greater than 0"},
		{"provider timeout", "      type: aliyun", "      <<: {timeout: 0s}", "dns.providers.ali-main.timeout: must be greater than 0"},
		{"qps", "      type: aliyun", "      <<: [{qps: 0}, {qps: 5}]", "dns.providers.ali-main.qps: must be at least 1"},
		{"ttl", "        rr: Home", "        <<: [{ttl: 0}, {ttl: 600}]", ".ttl: must be between 1 and 86400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(providerConfig, tc.anchor, tc.anchor+"\n"+tc.merge, 1)
			if _, err := loadString(t, input); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load error = %v, want merged zero validation error", err)
			}
		})
	}
}

func TestExplicitValuesOverrideMergedZero(t *testing.T) {
	input := strings.Replace(providerConfig, "routeros:", "routeros:\n  <<: &timeouts {timeout: 0s}\n  timeout: 250ms", 1)
	input = strings.Replace(input, "type: aliyun", "type: aliyun\n      <<: [*timeouts, {qps: 0}]\n      timeout: 5s\n      qps: 7", 1)
	input = strings.Replace(input, "rr: Home", "rr: Home\n        <<: {ttl: 0}\n        ttl: 300", 1)
	cfg, err := loadString(t, input)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.RouterOS.Timeout.Duration; got != 250*time.Millisecond {
		t.Errorf("routeros timeout = %s, want 250ms", got)
	}
	provider := cfg.DNS.Providers["ali-main"]
	if provider.Timeout.Duration != 5*time.Second || provider.QPS != 7 {
		t.Errorf("provider timeout = %s, qps = %d; want 5s, 7", provider.Timeout.Duration, provider.QPS)
	}
	if got := cfg.Items[0].Domains[0].TTL; got != 300 {
		t.Errorf("ttl = %d, want 300", got)
	}
}

func TestAliasedExplicitZeroRejected(t *testing.T) {
	providerInput := strings.Replace(providerConfig, "ali-main:", "ali-main: &provider", 1)
	providerInput = strings.Replace(providerInput, "accessKeySecret: ak-secret", "accessKeySecret: ak-secret\n      qps: 0\n    ali-copy:\n      <<: *provider", 1)
	domainInput := strings.Replace(providerConfig, "- provider: ali-main", "- &domain\n        provider: ali-main", 1)
	domainInput = strings.Replace(domainInput, "rr: Home", "rr: Home\n        ttl: 0\n      - <<: *domain\n        rr: backup", 1)
	for _, tc := range []struct {
		name    string
		input   string
		wantErr string
	}{
		{"provider", providerInput, "dns.providers.ali-copy.qps: must be at least 1"},
		{"domain", domainInput, ".domains[1].ttl: must be between 1 and 86400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadString(t, tc.input); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load error = %v, want aliased zero validation error", err)
			}
		})
	}
}
