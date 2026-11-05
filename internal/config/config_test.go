package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalConfig = `
routeros:
  url: https://192.168.88.1
  username: ddns
  password: super-secret
items:
  - name: wan-v6
    source:
      interface: wan
      family: ipv6
    addressList:
      list: ddns-wan-v6
`

const providerConfig = `
routeros:
  url: https://192.168.88.1
  username: ddns
  password: super-secret
dns:
  providers:
    ali-main:
      type: aliyun
      accessKeyId: ak-id
      accessKeySecret: ak-secret
items:
  - name: wan-v4
    source:
      interface: wan
      family: ipv4
    domains:
      - provider: ali-main
        domain: Example.COM.
        rr: Home
`

func loadString(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

func TestLoadMinimalDefaults(t *testing.T) {
	cfg, err := loadString(t, minimalConfig)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Runtime.PollInterval.Duration, 10*time.Second; got != want {
		t.Errorf("pollInterval = %s, want %s", got, want)
	}
	if got, want := cfg.Runtime.VerifyInterval.Duration, 10*time.Minute; got != want {
		t.Errorf("verifyInterval = %s, want %s", got, want)
	}
	if !cfg.Runtime.SyncOnStart {
		t.Error("syncOnStart = false, want true")
	}
	if got, want := cfg.State.Path, "./data/state.json"; got != want {
		t.Errorf("state.path = %q, want %q", got, want)
	}
	if got, want := cfg.API.Listen, "127.0.0.1:8080"; got != want {
		t.Errorf("api.listen = %q, want %q", got, want)
	}
	if cfg.Items[0].AddressList == nil || cfg.Items[0].AddressList.Comment != "ros-ddns:wan-v6" {
		t.Errorf("address list comment = %+v, want default ros-ddns:wan-v6", cfg.Items[0].AddressList)
	}
}

func TestDomainDefaultsAndNormalization(t *testing.T) {
	cfg, err := loadString(t, providerConfig)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Items[0].Domains[0]
	if got, want := d.Domain, "example.com"; got != want {
		t.Errorf("domain = %q, want %q", got, want)
	}
	if got, want := d.RR, "home"; got != want {
		t.Errorf("rr = %q, want %q", got, want)
	}
	if got, want := d.TTL, 600; got != want {
		t.Errorf("ttl = %d, want %d", got, want)
	}
	if got, want := d.Line, "default"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	if got, want := d.FQDN(), "home.example.com"; got != want {
		t.Errorf("fqdn = %q, want %q", got, want)
	}
}

func TestUnknownVersionRejected(t *testing.T) {
	_, err := loadString(t, "version: 1\n"+minimalConfig)
	if err == nil {
		t.Fatal("Load accepted a version field")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("error = %v, want a mention of version", err)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_ID", "env-id")
	cfgYAML := `
routeros:
  url: https://192.168.88.1
  username: ddns
  password: ${ROS_DDNS_TEST_ID}
dns:
  providers:
    ali-main:
      type: aliyun
      accessKeyId: ${ROS_DDNS_TEST_ID}
      accessKeySecret: ${ROS_DDNS_TEST_SECRET:-fallback-secret}
items:
  - name: wan-v4
    source:
      interface: wan
      family: ipv4
    domains:
      - provider: ali-main
        domain: example.com
        rr: "@"
`
	cfg, err := loadString(t, cfgYAML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.RouterOS.Password; got != "env-id" {
		t.Errorf("password = %q, want env-id", got)
	}
	if got := cfg.DNS.Providers["ali-main"].AccessKeySecret; got != "fallback-secret" {
		t.Errorf("accessKeySecret = %q, want fallback-secret", got)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("ROS_DDNS_TEST_SET", "value")
	t.Setenv("ROS_DDNS_TEST_EMPTY", "")

	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "plain", want: "plain"},
		{in: "${ROS_DDNS_TEST_SET}", want: "value"},
		{in: "${ROS_DDNS_TEST_MISSING:-fallback}", want: "fallback"},
		{in: "${ROS_DDNS_TEST_EMPTY:-fallback}", want: ""},
		{in: "${ROS_DDNS_TEST_MISSING}", wantErr: true},
		{in: "$${ROS_DDNS_TEST_SET}", want: "${ROS_DDNS_TEST_SET}"},
		{in: "$$", want: "$"},
		{in: "${unterminated", wantErr: true},
		{in: "${1BAD}", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ExpandEnv(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ExpandEnv(%q) succeeded, want error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ExpandEnv(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ExpandEnv(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEUI64OnlyForIPv6(t *testing.T) {
	yaml := strings.Replace(minimalConfig, "family: ipv6", "family: ipv4\n      eui64-mac: \"00:1a:2b:3c:4d:5e\"", 1)
	_, err := loadString(t, yaml)
	if err == nil {
		t.Fatal("Load accepted eui64-mac for ipv4")
	}
	if !strings.Contains(err.Error(), "eui64-mac") {
		t.Fatalf("error = %v, want a mention of eui64-mac", err)
	}
}

func TestInvalidEUI64MACRejected(t *testing.T) {
	yaml := strings.Replace(minimalConfig, "family: ipv6", "family: ipv6\n      eui64-mac: \"01:00:5e:00:00:01\"", 1)
	_, err := loadString(t, yaml)
	if err == nil {
		t.Fatal("Load accepted a multicast MAC")
	}
	if !strings.Contains(err.Error(), "eui64-mac") {
		t.Fatalf("error = %v, want a mention of eui64-mac", err)
	}
}

func TestEUI64MACValid(t *testing.T) {
	yaml := strings.Replace(minimalConfig, "family: ipv6", "family: ipv6\n      eui64-mac: \"00:1A:2B:3C:4D:5E\"", 1)
	cfg, err := loadString(t, yaml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Items[0].Source.EUI64MACAddr.String(), "00:1a:2b:3c:4d:5e"; got != want {
		t.Errorf("parsed MAC = %q, want %q", got, want)
	}
}

func TestDuplicateRecordTargetRejected(t *testing.T) {
	yaml := `
routeros:
  url: https://192.168.88.1
  username: ddns
  password: super-secret
dns:
  providers:
    ali-main:
      type: aliyun
      accessKeyId: ak-id
      accessKeySecret: ak-secret
items:
  - name: one
    source:
      interface: wan
      family: ipv4
    domains:
      - provider: ali-main
        domain: example.com
        rr: home
  - name: two
    source:
      interface: wan2
      family: ipv4
    domains:
      - provider: ali-main
        domain: example.com
        rr: home
`
	_, err := loadString(t, yaml)
	if err == nil {
		t.Fatal("Load accepted duplicate record targets")
	}
	if !strings.Contains(err.Error(), "duplicate record target") {
		t.Fatalf("error = %v, want duplicate record target", err)
	}
}

func TestEmptyItemsAccepted(t *testing.T) {
	const base = "routeros:\n  url: https://192.168.88.1\n  username: ddns\n"
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"omitted", base},
		{"explicitly empty", base + "dns:\n  providers: {}\nitems: []\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml), filepath.Join(t.TempDir(), "config.yaml"))
			if err != nil {
				t.Fatalf("Parse without items or providers: %v", err)
			}
			if len(cfg.Items) != 0 || len(cfg.DNS.Providers) != 0 {
				t.Fatalf("items=%d providers=%d, want 0 and 0", len(cfg.Items), len(cfg.DNS.Providers))
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate without items or providers: %v", err)
			}
		})
	}
}

func TestAPITokenRequiredForNonLoopback(t *testing.T) {
	_, err := loadString(t, "api:\n  listen: 0.0.0.0:8080\n"+minimalConfig)
	if err == nil || !strings.Contains(err.Error(), "api.token") {
		t.Fatalf("error = %v, want api.token requirement", err)
	}

	cfgYAML := "api:\n  listen: 0.0.0.0:8080\n  token: secret\n" + minimalConfig
	if _, err := loadString(t, cfgYAML); err != nil {
		t.Fatalf("Load with token: %v", err)
	}
}

func TestRouterOSURLValidation(t *testing.T) {
	userinfo := strings.Replace(minimalConfig, "https://192.168.88.1", "https://admin:hunter2@192.168.88.1", 1)
	if _, err := loadString(t, userinfo); err == nil || !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("error = %v, want userinfo rejection", err)
	}

	restPath := strings.Replace(minimalConfig, "https://192.168.88.1", "https://192.168.88.1/rest", 1)
	if _, err := loadString(t, restPath); err == nil || !strings.Contains(err.Error(), "/rest") {
		t.Fatalf("error = %v, want /rest rejection", err)
	}
}

func TestTrailingYAMLDocumentRejected(t *testing.T) {
	yaml := minimalConfig + "\n---\nlog:\n  level: debug\n"
	if _, err := loadString(t, yaml); err == nil || !strings.Contains(err.Error(), "one YAML document") {
		t.Fatalf("error = %v, want multi-document rejection", err)
	}
}

func TestAddressListTimeoutValidation(t *testing.T) {
	invalid := strings.Replace(minimalConfig, "list: ddns-wan-v6", "list: ddns-wan-v6\n      timeout: garbage", 1)
	if _, err := loadString(t, invalid); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error = %v, want invalid timeout rejection", err)
	}

	valid := strings.Replace(minimalConfig, "list: ddns-wan-v6", "list: ddns-wan-v6\n      timeout: 1h30m", 1)
	if _, err := loadString(t, valid); err != nil {
		t.Fatalf("Load valid timeout: %v", err)
	}
}

func TestRecordTypeFieldRejected(t *testing.T) {
	yaml := strings.Replace(providerConfig, "rr: Home", "rr: Home\n        type: A", 1)
	if _, err := loadString(t, yaml); err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("error = %v, want unknown type field rejection", err)
	}
}

func TestDuplicateAddressListTargetRejected(t *testing.T) {
	yaml := `
routeros:
  url: https://192.168.88.1
  username: ddns
  password: super-secret
items:
  - name: one
    source:
      interface: wan
      family: ipv6
    addressList:
      list: ddns
      comment: shared
  - name: two
    source:
      interface: wan2
      family: ipv6
    addressList:
      list: ddns
      comment: shared
`
	if _, err := loadString(t, yaml); err == nil || !strings.Contains(err.Error(), "duplicate address-list target") {
		t.Fatalf("error = %v, want duplicate address-list target", err)
	}
}

func TestAllItemsDisabledRejected(t *testing.T) {
	yaml := strings.Replace(minimalConfig, "name: wan-v6", "name: wan-v6\n    disabled: true", 1)
	if _, err := loadString(t, yaml); err == nil || !strings.Contains(err.Error(), "at least one item must be enabled") {
		t.Fatalf("error = %v, want all-disabled rejection", err)
	}
}

func TestEUI64MACCanonicalized(t *testing.T) {
	yaml := strings.Replace(minimalConfig, "family: ipv6", "family: ipv6\n      eui64-mac: \"00-1A-2B-3C-4D-5E\"", 1)
	cfg, err := loadString(t, yaml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Items[0].Source.EUI64MAC, "00:1a:2b:3c:4d:5e"; got != want {
		t.Errorf("canonical MAC = %q, want %q", got, want)
	}
}
