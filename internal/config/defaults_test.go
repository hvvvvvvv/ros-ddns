package config

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigYAML(t *testing.T) {
	raw := DefaultConfigYAML()
	if len(raw) == 0 {
		t.Fatal("default configuration is empty")
	}
	// The default must be self-contained: a service start has no access to a
	// shell environment, so unresolved references would break the first start.
	if bytes.Contains(raw, []byte("${")) {
		t.Fatalf("default configuration contains an environment reference:\n%s", raw)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := Parse(raw, path)
	if err != nil {
		t.Fatalf("parse default configuration: %v\n%s", err, raw)
	}
	if cfg.API.Listen != "127.0.0.1:8080" {
		t.Errorf("api.listen = %q", cfg.API.Listen)
	}
	if cfg.API.Token != "" {
		t.Errorf("api.token = %q, want empty", cfg.API.Token)
	}
	if cfg.State.Path != "state.json" {
		t.Errorf("state.path = %q", cfg.State.Path)
	}
	if len(cfg.Items) != 1 || len(cfg.DNS.Providers) != 1 {
		t.Fatalf("items=%d providers=%d, want 1 and 1", len(cfg.Items), len(cfg.DNS.Providers))
	}
	provider := cfg.DNS.Providers["ali-main"]
	if provider.AccessKeyID != "" || provider.AccessKeySecret != "" {
		t.Errorf("default provider credentials should be empty placeholders: %+v", provider)
	}
	if cfg.RouterOS.TLS.InsecureSkipVerify != true {
		t.Error("default routeros TLS should skip verification for the first start")
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
