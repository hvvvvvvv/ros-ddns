package config

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func clearDefaultConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"ROS_DDNS_LISTEN",
		"ROS_DDNS_API_TOKEN",
		"ROS_DDNS_ROUTEROS_URL",
		"ROS_DDNS_ROUTEROS_USERNAME",
		"ROS_PASSWORD",
		"ROS_DDNS_CA_FILE",
		"ROS_DDNS_LOG_LEVEL",
		"ROS_DDNS_LOG_FORMAT",
		"ROS_DDNS_POLL_INTERVAL",
		"ROS_DDNS_STABLE_FOR",
		"ROS_DDNS_RECONCILE_TIMEOUT",
		"ROS_DDNS_VERIFY_INTERVAL",
		"ROS_DDNS_SYNC_ON_START",
		"ROS_DDNS_MAX_BACKOFF",
		"ROS_DDNS_STATE_PATH",
	} {
		value, wasSet := os.LookupEnv(name)
		t.Cleanup(func() {
			var err error
			if wasSet {
				err = os.Setenv(name, value)
			} else {
				err = os.Unsetenv(name)
			}
			if err != nil {
				t.Errorf("restore environment variable %s: %v", name, err)
			}
		})
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset environment variable %s: %v", name, err)
		}
	}
}

func TestDefaultConfigYAML(t *testing.T) {
	clearDefaultConfigEnv(t)
	raw := DefaultConfigYAML()
	if len(raw) == 0 {
		t.Fatal("default configuration is empty")
	}
	// Every reference needs a fallback so a service can start without a shell
	// environment.
	references := regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:-[^}]*\}`).FindAll(raw, -1)
	if len(references) != bytes.Count(raw, []byte("${")) {
		t.Fatalf("default configuration contains an environment reference without a fallback:\n%s", raw)
	}
	if regexp.MustCompile(`(?m)^(dns|items):`).Match(raw) {
		t.Fatal("default configuration must omit the dns and items sections")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := Parse(raw, path)
	if err != nil {
		t.Fatalf("parse default configuration: %v\n%s", err, raw)
	}
	if len(cfg.Items) != 0 || len(cfg.DNS.Providers) != 0 {
		t.Fatalf("items=%d providers=%d, want 0 and 0", len(cfg.Items), len(cfg.DNS.Providers))
	}
	for _, field := range []struct{ name, got, want string }{
		{"api.listen", cfg.API.Listen, "127.0.0.1:8080"},
		{"api.token", cfg.API.Token, ""},
		{"state.path", cfg.State.Path, "state.json"},
		{"routeros.url", cfg.RouterOS.URL, "https://192.168.88.1"},
		{"routeros.username", cfg.RouterOS.Username, "admin"},
		{"routeros.password", cfg.RouterOS.Password, ""},
		{"routeros.tls.caFile", cfg.RouterOS.TLS.CAFile, ""},
	} {
		if field.got != field.want {
			t.Errorf("%s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if cfg.RouterOS.TLS.InsecureSkipVerify {
		t.Error("default routeros TLS should verify certificates")
	}
	if got := cfg.RouterOS.Timeout.Duration; got != 10*time.Second {
		t.Errorf("default routeros.timeout = %s, want 10s", got)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Errorf("default log = %q/%q, want info/text", cfg.Log.Level, cfg.Log.Format)
	}
	if cfg.Runtime.PollInterval.Duration != 10*time.Second ||
		cfg.Runtime.StableFor.Duration != 0 ||
		cfg.Runtime.ReconcileTimeout.Duration != 30*time.Second ||
		cfg.Runtime.VerifyInterval.Duration != 10*time.Minute ||
		!cfg.Runtime.SyncOnStart ||
		cfg.Runtime.MaxBackoff.Duration != 5*time.Minute {
		t.Errorf("default runtime = %+v", cfg.Runtime)
	}
	if summary := Summary(cfg); !strings.HasSuffix(summary, "providers:\n  (none)\nitems:\n") {
		t.Errorf("default summary must show empty providers and items: %q", summary)
	}
}

func TestDefaultConfigEnvOverrides(t *testing.T) {
	clearDefaultConfigEnv(t)
	t.Setenv("ROS_DDNS_LISTEN", "0.0.0.0:8080")
	t.Setenv("ROS_DDNS_API_TOKEN", "token")
	t.Setenv("ROS_DDNS_ROUTEROS_URL", "https://router.example")
	t.Setenv("ROS_DDNS_ROUTEROS_USERNAME", "ddns")
	t.Setenv("ROS_PASSWORD", "pw")
	t.Setenv("ROS_DDNS_CA_FILE", "/data/ca.pem")
	t.Setenv("ROS_DDNS_LOG_LEVEL", "debug")
	t.Setenv("ROS_DDNS_LOG_FORMAT", "json")
	t.Setenv("ROS_DDNS_POLL_INTERVAL", "2s")
	t.Setenv("ROS_DDNS_STABLE_FOR", "5s")
	t.Setenv("ROS_DDNS_RECONCILE_TIMEOUT", "15s")
	t.Setenv("ROS_DDNS_VERIFY_INTERVAL", "0s")
	t.Setenv("ROS_DDNS_SYNC_ON_START", "false")
	t.Setenv("ROS_DDNS_MAX_BACKOFF", "1m")
	t.Setenv("ROS_DDNS_STATE_PATH", "/tmp/custom-state.json")

	cfg, err := Parse(DefaultConfigYAML(), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("parse default configuration with environment overrides: %v", err)
	}
	if len(cfg.Items) != 0 || len(cfg.DNS.Providers) != 0 {
		t.Fatalf("items=%d providers=%d, want 0 and 0", len(cfg.Items), len(cfg.DNS.Providers))
	}
	for _, field := range []struct{ name, got, want string }{
		{"api.listen", cfg.API.Listen, "0.0.0.0:8080"},
		{"api.token", cfg.API.Token, "token"},
		{"routeros.url", cfg.RouterOS.URL, "https://router.example"},
		{"routeros.username", cfg.RouterOS.Username, "ddns"},
		{"routeros.password", cfg.RouterOS.Password, "pw"},
		{"routeros.tls.caFile", cfg.RouterOS.TLS.CAFile, "/data/ca.pem"},
		{"log.level", cfg.Log.Level, "debug"},
		{"log.format", cfg.Log.Format, "json"},
		{"state.path", cfg.State.Path, "/tmp/custom-state.json"},
	} {
		if field.got != field.want {
			t.Errorf("%s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if cfg.RouterOS.TLS.InsecureSkipVerify {
		t.Error("routeros TLS should still verify certificates with environment overrides")
	}
	if got := cfg.RouterOS.Timeout.Duration; got != 10*time.Second {
		t.Errorf("routeros.timeout with environment overrides = %s, want 10s", got)
	}
	if cfg.Runtime.PollInterval.Duration != 2*time.Second ||
		cfg.Runtime.StableFor.Duration != 5*time.Second ||
		cfg.Runtime.ReconcileTimeout.Duration != 15*time.Second ||
		cfg.Runtime.VerifyInterval.Duration != 0 ||
		cfg.Runtime.SyncOnStart ||
		cfg.Runtime.MaxBackoff.Duration != time.Minute {
		t.Errorf("overridden runtime = %+v", cfg.Runtime)
	}
}

// Provider credentials may be absent: a missing or invalid key must not stop the
// daemon from starting, only make synchronization fail and retry.
func TestProviderCredentialsOptional(t *testing.T) {
	raw := []byte(`
api:
  listen: 127.0.0.1:8080
routeros:
  url: https://192.168.88.1
  username: admin
dns:
  providers:
    ali-main:
      type: aliyun
      endpoint: alidns.aliyuncs.com
      accessKeyId: ""
      accessKeySecret: ""
items:
  - name: home-ipv4
    source:
      interface: wan
      family: ipv4
    domains:
      - provider: ali-main
        domain: example.com
        rr: home
`)
	cfg, err := Parse(raw, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("empty provider credentials must be accepted: %v", err)
	}
	provider := cfg.DNS.Providers["ali-main"]
	if provider.AccessKeyID != "" || provider.AccessKeySecret != "" {
		t.Fatalf("provider = %+v", provider)
	}
	if summary := Summary(cfg); !strings.Contains(summary, "accessKeyId=unset") || !strings.Contains(summary, "accessKeySecret=unset") {
		t.Fatalf("summary = %q", summary)
	}
}
