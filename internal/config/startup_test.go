package config

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func startupYAMLField(path, scalar string) string {
	section, key, _ := strings.Cut(path, ".")
	return section + ":\n  " + key + ": " + scalar + "\n"
}

func mustStartup(t *testing.T, raw string) *Startup {
	t.Helper()
	startup, err := ParseStartup([]byte(raw), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("ParseStartup: %v", err)
	}
	return startup
}

func startupValue(c *Config, path string) string {
	switch path {
	case "log.level":
		return c.Log.Level
	case "log.format":
		return c.Log.Format
	case "runtime.pollInterval":
		return c.Runtime.PollInterval.Duration.String()
	case "runtime.stableFor":
		return c.Runtime.StableFor.Duration.String()
	case "runtime.reconcileTimeout":
		return c.Runtime.ReconcileTimeout.Duration.String()
	case "runtime.verifyInterval":
		return c.Runtime.VerifyInterval.Duration.String()
	case "runtime.maxBackoff":
		return c.Runtime.MaxBackoff.Duration.String()
	case "state.path":
		return c.State.Path
	case "api.listen":
		return c.API.Listen
	case "api.token":
		return c.API.Token
	default:
		panic("unexpected startup test path")
	}
}

func TestParseStartupValidAndAbsentFields(t *testing.T) {
	for _, raw := range []string{
		"{}", "log: {}\nruntime: {}\nstate: {}\napi: {}\n",
		"log: null\nruntime: null\nstate: null\napi: null\n",
		"log: {level: null, format: null}\nruntime: {pollInterval: null, stableFor: null, reconcileTimeout: null, verifyInterval: null, syncOnStart: null, maxBackoff: null}\nstate: {path: null}\napi: {listen: null, token: null}\n",
		minimalConfig, providerConfig,
	} {
		t.Run(raw, func(t *testing.T) {
			startup := mustStartup(t, raw)
			if len(startup.Fixups) != 0 || startup.NeedsWrite() {
				t.Fatalf("valid/defaulted configuration has fixups: %+v", startup.Fixups)
			}
			defaults := Default()
			for _, path := range []string{"log.level", "log.format", "runtime.pollInterval", "runtime.stableFor", "runtime.reconcileTimeout", "runtime.verifyInterval", "runtime.maxBackoff", "state.path", "api.listen", "api.token"} {
				if got, want := startupValue(startup.Config, path), startupValue(defaults, path); got != want {
					t.Errorf("%s = %q, want %q", path, got, want)
				}
			}
			if !startup.Config.Runtime.SyncOnStart || startup.Config.Path == "" {
				t.Error("default syncOnStart or source path lost")
			}
		})
	}
	startup := mustStartup(t, "log: {level: ' DEBUG ', format: ' JSON '}\napi: {listen: '  localhost:8080  ', token: ' token '}\nstate: {path: ''}\nruntime: {syncOnStart: false, stableFor: 0s, verifyInterval: 0s}\n")
	if len(startup.Fixups) != 0 || startup.Config.Log.Level != "debug" || startup.Config.Log.Format != "json" || startup.Config.API.Listen != "localhost:8080" || startup.Config.API.Token != "token" || startup.Config.State.Path != "" || startup.Config.Runtime.SyncOnStart {
		t.Fatal("valid explicit values or normalization changed")
	}
}

func TestParseStartupLiteralFallbacks(t *testing.T) {
	for _, tc := range []struct{ path, input, want string }{
		{"log.level", "verbose", "info"}, {"log.level", `""`, "info"},
		{"log.format", "xml", "text"}, {"log.format", `""`, "text"},
		{"runtime.pollInterval", "0s", "10s"}, {"runtime.pollInterval", "-1s", "10s"},
		{"runtime.stableFor", "-1s", "0s"},
		{"runtime.reconcileTimeout", "0s", "30s"}, {"runtime.reconcileTimeout", "-1s", "30s"},
		{"runtime.verifyInterval", "-1s", "10m0s"},
		{"runtime.maxBackoff", "-1s", "5m0s"}, {"runtime.maxBackoff", "0s", "5m0s"}, {"runtime.maxBackoff", "1s", "5m0s"},
		{"api.listen", `""`, "127.0.0.1:8080"}, {"api.listen", `" \t "`, "127.0.0.1:8080"},
		{"state.path", `"bad\x00path"`, "./data/state.json"},
	} {
		t.Run(tc.path+"/"+tc.input, func(t *testing.T) {
			startup := mustStartup(t, startupYAMLField(tc.path, tc.input))
			if got := startupValue(startup.Config, tc.path); got != tc.want {
				t.Errorf("effective value = %q, want %q", got, tc.want)
			}
			if len(startup.Fixups) != 1 {
				t.Fatalf("fixups = %+v, want one", startup.Fixups)
			}
			fixup := startup.Fixups[0]
			if fixup.Path != tc.path || fixup.Value != tc.want || fixup.Reason == "" || fixup.Secret || !fixup.Write || !startup.NeedsWrite() {
				t.Errorf("incorrect fallback: %+v", fixup)
			}
		})
	}
}

func TestParseStartupMaxBackoffUsesEffectivePoll(t *testing.T) {
	for _, tc := range []struct {
		poll, backoff, want string
		fixups              int
	}{
		{"0s", "1s", "5m0s", 2}, {"10m", "1m", "10m0s", 1},
		{"10m", "15m", "15m0s", 0},
		{"10m", "null", "10m0s", 1},
		{"10h", "", "10h0m0s", 1}, {"10h", "null", "10h0m0s", 1},
	} {
		t.Run(tc.poll+"/"+tc.backoff, func(t *testing.T) {
			raw := startupYAMLField("runtime.pollInterval", tc.poll)
			if tc.backoff != "" {
				raw += "  maxBackoff: " + tc.backoff + "\n"
			}
			startup := mustStartup(t, raw)
			if startup.Config.Runtime.MaxBackoff.Duration.String() != tc.want || len(startup.Fixups) != tc.fixups {
				t.Fatalf("maxBackoff = %s, fixups = %+v", startup.Config.Runtime.MaxBackoff.Duration, startup.Fixups)
			}
			if tc.fixups > 0 {
				want := Fixup{
					Path: "runtime.maxBackoff", Value: tc.want, Write: true,
					Reason: "must be greater than or equal to runtime.pollInterval",
				}
				found := false
				for _, fixup := range startup.Fixups {
					if fixup == want {
						found = true
					}
				}
				if !found || !startup.NeedsWrite() {
					t.Fatalf("missing writable maxBackoff fixup: %+v", startup.Fixups)
				}
			}
		})
	}
}

func TestParseStartupMaxBackoffPreservesReferences(t *testing.T) {
	t.Setenv("STARTUP_BACKOFF", "1m")
	for _, raw := range []string{
		"runtime: {pollInterval: 10h, maxBackoff: '${STARTUP_BACKOFF}'}",
		"runtime: {pollInterval: 10h, stableFor: &backoff 1m, maxBackoff: *backoff}",
		"runtime: {pollInterval: 10h, <<: {maxBackoff: 1m}}",
		"<<: {runtime: &runtime {pollInterval: 10h, maxBackoff: 1m}}\nruntime: *runtime",
	} {
		t.Run(raw, func(t *testing.T) {
			startup := mustStartup(t, raw)
			want := Fixup{
				Path: "runtime.maxBackoff", Value: "10h0m0s", Write: false,
				Reason: "must be greater than or equal to runtime.pollInterval",
			}
			if startup.Config.Runtime.MaxBackoff.Duration != 10*time.Hour || len(startup.Fixups) != 1 || startup.Fixups[0] != want || startup.NeedsWrite() {
				t.Fatalf("reference fallback = %+v, maxBackoff = %s", startup.Fixups, startup.Config.Runtime.MaxBackoff.Duration)
			}
			out, err := ApplyStartupFixups([]byte(raw), startup.Fixups)
			if err != nil || string(out) != raw {
				t.Fatalf("reference was rewritten: err=%v, output=%s", err, out)
			}
		})
	}
}

func TestParseStartupStatePath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	for _, path := range []string{configPath, configPath + ".bak"} {
		startup, err := ParseStartup([]byte(startupYAMLField("state.path", fmt.Sprintf("%q", path))), configPath)
		if err != nil || startup.Config.State.Path != "./data/state.json" || len(startup.Fixups) != 1 || !startup.Fixups[0].Write {
			t.Fatalf("state collision fallback: startup=%+v, err=%v", startup, err)
		}
	}
	for _, scalar := range []string{`""`, `" \t "`} {
		startup := mustStartup(t, startupYAMLField("state.path", scalar))
		if startup.Config.State.Path != "" || len(startup.Fixups) != 0 {
			t.Fatal("memory-only state must remain empty without fixups")
		}
	}
}

func TestParseStartupEnvironmentFallbacks(t *testing.T) {
	for _, tc := range []struct{ path, value, want string }{
		{"log.level", "secret-unsupported-level", "info"}, {"log.format", "", "text"},
		{"runtime.pollInterval", "0s", "10s"}, {"runtime.stableFor", "-1s", "0s"},
		{"runtime.reconcileTimeout", "0s", "30s"}, {"runtime.verifyInterval", "-1s", "10m0s"},
		{"runtime.maxBackoff", "1s", "5m0s"}, {"api.listen", "", "127.0.0.1:8080"},
		{"api.token", "secret token", ""}, {"state.path", "bad\x00path", "./data/state.json"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			// Environment variables cannot contain NUL; use a path collision.
			path := filepath.Join(t.TempDir(), "config.yaml")
			value := tc.value
			if tc.path == "state.path" {
				value = path
			}
			t.Setenv("STARTUP_TEST_VALUE", value)
			startup, err := ParseStartup([]byte(startupYAMLField(tc.path, "${STARTUP_TEST_VALUE}")), path)
			if err != nil {
				t.Fatalf("ParseStartup: %v", err)
			}
			if startupValue(startup.Config, tc.path) != tc.want || len(startup.Fixups) != 1 || startup.Fixups[0].Write || startup.NeedsWrite() {
				t.Fatal("environment fallback must take effect without writeback")
			}
			if strings.Contains(startup.Fixups[0].Reason, "secret") || strings.Contains(startup.Fixups[0].Reason, path) {
				t.Fatal("fallback reason includes submitted data")
			}
		})
	}
	startup := mustStartup(t, "log: {level: '$${LITERAL_REFERENCE}'}")
	if len(startup.Fixups) != 1 || startup.Fixups[0].Write || startup.Config.Log.Level != "info" {
		t.Fatal("escaped reference must also be preserved")
	}
}

func TestParseStartupTokenRules(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "[::]:8080", "router.invalid:8080"} {
		for _, token := range []string{"", "null", `""`, `"bad token"`, `"bad\tvalue"`, `"bad\u007fvalue"`, `"中文"`, fmt.Sprintf("%q", strings.Repeat("x", 257)), "${STARTUP_TEST_TOKEN}"} {
			t.Run(listen+"/"+token, func(t *testing.T) {
				t.Setenv("STARTUP_TEST_TOKEN", "")
				raw := startupYAMLField("api.listen", fmt.Sprintf("%q", listen))
				if token != "" {
					raw += "  token: " + token + "\n"
				}
				startup := mustStartup(t, raw)
				decoded, err := hex.DecodeString(startup.Config.API.Token)
				if err != nil || len(decoded) != 32 || len(startup.Config.API.Token) != 64 {
					t.Fatal("generated token must contain 32 random bytes encoded as 64 hex characters")
				}
				if len(startup.Fixups) != 1 {
					t.Fatal("expected one generated token fixup")
				}
				fixup := startup.Fixups[0]
				if fixup.Path != "api.token" || fixup.Value != startup.Config.API.Token || !fixup.Secret || !fixup.Write || !startup.NeedsWrite() || strings.Contains(fixup.Reason, startup.Config.API.Token) {
					t.Fatal("incorrect generated token fixup metadata")
				}
			})
		}
	}
	for _, listen := range []string{"127.0.0.1:8080", "[::1]:8080", "LOCALHOST:8080", "[::ffff:127.0.0.1]:8080"} {
		for _, token := range []string{"", "null", `""`, "valid-token", `"bad token"`} {
			t.Run(listen+"/"+token, func(t *testing.T) {
				raw := startupYAMLField("api.listen", fmt.Sprintf("%q", listen))
				if token != "" {
					raw += "  token: " + token + "\n"
				}
				startup := mustStartup(t, raw)
				if token == `"bad token"` {
					if startup.Config.API.Token != "" || len(startup.Fixups) != 1 || !startup.Fixups[0].Write || !startup.Fixups[0].Secret {
						t.Fatal("invalid local token must fall back to empty and be writable")
					}
				} else if len(startup.Fixups) != 0 {
					t.Fatal("valid/absent local token must not create a fixup")
				}
			})
		}
	}
	startup := mustStartup(t, "api: {listen: '0.0.0.0:8080', token: valid-token}")
	if startup.Config.API.Token != "valid-token" || len(startup.Fixups) != 0 {
		t.Fatal("valid non-loopback token changed")
	}
}

func TestParseStartupReplacesInvalidEnvironmentToken(t *testing.T) {
	const raw = "api: {listen: '0.0.0.0:8080', token: '${STARTUP_ROTATE_TOKEN}'}"
	for _, value := range []string{"", "invalid token"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("STARTUP_ROTATE_TOKEN", value)
			startup := mustStartup(t, raw)
			if len(startup.Fixups) != 1 || !startup.Fixups[0].Write || !startup.Fixups[0].Secret {
				t.Fatal("generated token must always be writable and secret")
			}
			out, err := ApplyStartupFixups([]byte(raw), startup.Fixups)
			if err != nil || strings.Contains(string(out), "${") {
				t.Fatalf("generated token did not replace the placeholder: %v", err)
			}
			again := mustStartup(t, string(out))
			if len(again.Fixups) != 0 || again.Config.API.Token != startup.Config.API.Token {
				t.Fatal("generated token did not survive writeback")
			}
		})
	}
}

func TestParseStartupFatalListen(t *testing.T) {
	for _, listen := range []string{"missing-port", "127.0.0.1:", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:http", "127.0.0.1:+80", "127.0.0.1:8 0", "::1:8080"} {
		t.Run(listen, func(t *testing.T) {
			if _, err := ParseStartup([]byte(startupYAMLField("api.listen", fmt.Sprintf("%q", listen))), ""); err == nil || !strings.Contains(err.Error(), "api.listen") {
				t.Fatalf("error = %v, want api.listen error", err)
			}
		})
	}
}

func TestParseStartupFatalDecodeMatchesParse(t *testing.T) {
	for _, raw := range []string{
		"", "  # empty\n", "api: [", "unknown: true", "runtime: {unknown: true}",
		"log: {level: info, level: debug}", "dns: {providers: {a: {type: aliyun, unknown: true}}}",
		"runtime: {pollInterval: 1d}", "routeros: {timeout: nonsense}",
		"items: [{domains: [{ttl: wrong}]}]", "runtime: {syncOnStart: []}",
		"api: {token: {nested: value}}", "{}\n---\n{}", "{}\n---\n", "{}\n---\n[",
		"api: &recursive {<<: *recursive}",
	} {
		t.Run(raw, func(t *testing.T) {
			_, strictErr := Parse([]byte(raw), "")
			_, startupErr := ParseStartup([]byte(raw), "")
			if strictErr == nil || startupErr == nil || strictErr.Error() != startupErr.Error() {
				t.Fatalf("strict error = %v; startup error = %v", strictErr, startupErr)
			}
		})
	}
}

func TestParseStartupRedactsEnvironmentErrors(t *testing.T) {
	t.Setenv("STARTUP_TEST_SECRET", "SecretLeakProbe")
	for _, raw := range []string{
		"runtime: {pollInterval: '${STARTUP_TEST_SECRET}'}",
		"api: {listen: '${STARTUP_TEST_SECRET}'}",
		"items: [{domains: [{ttl: '${STARTUP_TEST_SECRET}'}]}]",
		"runtime: {syncOnStart: '${STARTUP_TEST_SECRET}'}",
		"routeros: {password: '${STARTUP_TEST_SECRET}'}\napi: {token: '${1BAD}'}",
	} {
		_, err := ParseStartup([]byte(raw), "")
		if err == nil || strings.Contains(err.Error(), "SecretLeakProbe") || strings.Contains(err.Error(), "SecretL...") {
			t.Fatalf("expected a safe error, got %v", err)
		}
	}
}

func TestParseStartupSkipsOptionalSemantics(t *testing.T) {
	for _, tc := range []struct {
		raw          string
		strictAccept bool
	}{
		{"{}", true}, {"items: []", true}, {"items: [{name: paused, disabled: true}]", true},
		{"items: [{disabled: true}]", false}, {"items: [{}]", false},
		{"routeros: {url: bad-url, timeout: 0s, tls: {insecureSkipVerify: true, caFile: bad}}", false},
		{"dns: {providers: {bad: {type: unsupported, timeout: -1s, qps: 0}}}", false},
		{"items: [{name: bad-name, source: {family: other, eui64-mac: bad, select: {prefix: bad, index: -1}}, domains: [{ttl: 0}], addressList: {timeout: invalid}}]", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			startup := mustStartup(t, tc.raw)
			if len(startup.Fixups) != 0 {
				t.Fatalf("optional parameters generated fixups: %+v", startup.Fixups)
			}
			if _, err := Parse([]byte(tc.raw), startup.Config.Path); (err == nil) != tc.strictAccept {
				t.Fatalf("strict Parse error = %v, want accepted=%t", err, tc.strictAccept)
			}
		})
	}
}

func TestParseStartupAliasAndMergeFallbacks(t *testing.T) {
	for _, raw := range []string{
		"routeros: {password: &bad invalid}\nlog: {level: *bad}",
		"log: {<<: {level: invalid}}",
		"<<: {log: &logging {level: invalid}}\nlog: *logging",
		"log: {<<: [{level: invalid}, {level: info}]}",
	} {
		startup := mustStartup(t, raw)
		if startup.Config.Log.Level != "info" || len(startup.Fixups) != 1 || startup.Fixups[0].Write {
			t.Fatalf("alias/merge fallback = %+v", startup.Fixups)
		}
	}
	startup := mustStartup(t, "log: {<<: {level: invalid}, level: null}")
	if len(startup.Fixups) != 0 || startup.Config.Log.Level != "info" {
		t.Fatal("explicit null must override merge and retain default")
	}
	startup = mustStartup(t, "runtime: {pollInterval: 250ms, stableFor: 0s, verifyInterval: 0s, maxBackoff: 1s}")
	before := startup.Config.Runtime
	startup.Config.Normalize()
	if !reflect.DeepEqual(before, startup.Config.Runtime) || before.PollInterval.Duration != 250*time.Millisecond {
		t.Fatal("startup configuration must tolerate repeated normalization")
	}
}
