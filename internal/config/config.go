// Package config defines the ros-ddns configuration schema, defaults,
// environment expansion and semantic validation.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	FamilyIPv4 = "ipv4"
	FamilyIPv6 = "ipv6"

	ScopePublic = "public"
	ScopeAny    = "any"
)

// RecordType returns the DNS record type implied by an address family.
func RecordType(family string) string {
	if family == FamilyIPv6 {
		return "AAAA"
	}
	return "A"
}

// Duration is a time.Duration that marshals to/from strings like "10s".
type Duration struct {
	time.Duration
	set bool // A non-null value was explicitly supplied in YAML.
}

// UnmarshalYAML parses durations exclusively as strings, e.g. "10s", "5m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	d.set = true
	return nil
}

// MarshalYAML renders the duration in its string form.
func (d Duration) MarshalYAML() (any, error) {
	return d.Duration.String(), nil
}

// Config is the root configuration object.
type Config struct {
	Log      LogConfig      `yaml:"log"`
	Runtime  RuntimeConfig  `yaml:"runtime"`
	State    StateConfig    `yaml:"state"`
	API      APIConfig      `yaml:"api"`
	RouterOS RouterOSConfig `yaml:"routeros"`
	DNS      DNSConfig      `yaml:"dns"`
	Items    []ItemConfig   `yaml:"items"`

	// Path is the config file the values were loaded from (not serialized).
	Path string `yaml:"-"`
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// RuntimeConfig controls polling, debouncing, reconciliation and retries.
type RuntimeConfig struct {
	PollInterval     Duration `yaml:"pollInterval"`
	StableFor        Duration `yaml:"stableFor"`
	ReconcileTimeout Duration `yaml:"reconcileTimeout"`
	VerifyInterval   Duration `yaml:"verifyInterval"`
	SyncOnStart      bool     `yaml:"syncOnStart"`
	MaxBackoff       Duration `yaml:"maxBackoff"`
}

// StateConfig controls the optional JSON state file.
type StateConfig struct {
	Path string `yaml:"path"`
}

// APIConfig controls the local HTTP API.
type APIConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

// RouterOSConfig is the single RouterOS REST endpoint.
type RouterOSConfig struct {
	URL      string    `yaml:"url"`
	Username string    `yaml:"username"`
	Password string    `yaml:"password"`
	Timeout  Duration  `yaml:"timeout"`
	TLS      TLSConfig `yaml:"tls"`
}

// TLSConfig controls HTTPS verification for the RouterOS connection.
type TLSConfig struct {
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify"`
	CAFile             string `yaml:"caFile"`
}

// DNSConfig groups DNS provider configurations.
type DNSConfig struct {
	Providers map[string]ProviderConfig `yaml:"providers"`
}

// ProviderConfig is a DNS provider instance (currently only aliyun).
type ProviderConfig struct {
	Type            string   `yaml:"type"`
	Endpoint        string   `yaml:"endpoint"`
	RegionID        string   `yaml:"regionId"`
	AccessKeyID     string   `yaml:"accessKeyId"`
	AccessKeySecret string   `yaml:"accessKeySecret"`
	SecurityToken   string   `yaml:"securityToken"`
	Timeout         Duration `yaml:"timeout"`
	QPS             int      `yaml:"qps"`
	qpsSet          bool
}

// UnmarshalYAML tracks explicit QPS values without changing the public int field.
// The callback form retains the caller's KnownFields setting, unlike Node.Decode.
func (p *ProviderConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type plain ProviderConfig
	if err := unmarshal((*plain)(p)); err != nil {
		return err
	}
	set, err := yamlFieldSet(unmarshal, "qps")
	if err != nil {
		return err
	}
	p.qpsSet = set
	return nil
}

// ItemConfig binds one RouterOS address source to DNS records and an optional
// address-list target.
type ItemConfig struct {
	Name        string             `yaml:"name"`
	Disabled    bool               `yaml:"disabled"`
	Source      SourceConfig       `yaml:"source"`
	Domains     []DomainConfig     `yaml:"domains"`
	AddressList *AddressListConfig `yaml:"addressList"`
}

// SourceConfig describes where the record value comes from.
type SourceConfig struct {
	Interface    string           `yaml:"interface"`
	Family       string           `yaml:"family"`
	EUI64MAC     string           `yaml:"eui64-mac"`
	EUI64MACAddr net.HardwareAddr `yaml:"-"`
	Select       SelectConfig     `yaml:"select"`
}

// SelectConfig narrows the interface address candidates.
type SelectConfig struct {
	Scope     string        `yaml:"scope"`
	Prefix    string        `yaml:"prefix"`
	PrefixNet *netip.Prefix `yaml:"-"`
	Index     int           `yaml:"index"`
}

// DomainConfig is one DNS record binding for the item value.
type DomainConfig struct {
	Provider string `yaml:"provider"`
	Domain   string `yaml:"domain"`
	RR       string `yaml:"rr"`
	TTL      int    `yaml:"ttl"`
	Line     string `yaml:"line"`
	ttlSet   bool
}

// UnmarshalYAML tracks explicit TTL values while retaining strict field checks.
func (d *DomainConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type plain DomainConfig
	if err := unmarshal((*plain)(d)); err != nil {
		return err
	}
	set, err := yamlFieldSet(unmarshal, "ttl")
	if err != nil {
		return err
	}
	d.ttlSet = set
	return nil
}

func yamlFieldSet(unmarshal func(any) error, name string) (bool, error) {
	// Let the YAML decoder resolve aliases and merge keys. Null keeps the
	// existing absent-field defaulting behavior.
	var fields map[string]any
	if err := unmarshal(&fields); err != nil {
		return false, err
	}
	return fields[name] != nil, nil
}

// FQDN returns the fully qualified name for this binding.
func (d DomainConfig) FQDN() string {
	if d.RR == "@" {
		return d.Domain
	}
	return d.RR + "." + d.Domain
}

// AddressListConfig describes the optional IPv4/IPv6 address-list target.
type AddressListConfig struct {
	List    string `yaml:"list"`
	Timeout string `yaml:"timeout"`
	Comment string `yaml:"comment"`
}

// Default returns a config pre-populated with every scalar default. YAML
// decoding into this value preserves defaults for absent fields.
func Default() *Config {
	return &Config{
		Log: LogConfig{Level: "info", Format: "text"},
		Runtime: RuntimeConfig{
			PollInterval:     Duration{Duration: 10 * time.Second},
			StableFor:        Duration{},
			ReconcileTimeout: Duration{Duration: 30 * time.Second},
			VerifyInterval:   Duration{Duration: 10 * time.Minute},
			SyncOnStart:      true,
			MaxBackoff:       Duration{Duration: 5 * time.Minute},
		},
		State: StateConfig{Path: "./data/state.json"},
		API:   APIConfig{Listen: "127.0.0.1:8080"},
		RouterOS: RouterOSConfig{
			Timeout: Duration{Duration: 10 * time.Second},
		},
		DNS: DNSConfig{Providers: map[string]ProviderConfig{}},
	}
}
