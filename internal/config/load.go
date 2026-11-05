package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads, parses, expands and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := Parse(data, path)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse parses configuration bytes before expanding scalar values. Expanded
// scalars are strings; numeric fields must use YAML literals, while direct
// boolean fields accept environment-expanded true/false values.
// It then applies defaults and validation. The path is used for Config.Path.
func Parse(data []byte, path string) (*Config, error) {
	document, err := parseYAMLDocument(data)
	if err != nil {
		return nil, err
	}
	cfg, values, err := decodeConfig(document, path)
	if err != nil {
		return nil, err
	}
	listen := cfg.API.Listen
	cfg.Normalize()
	// Strict loading has always rejected whitespace in the port. Preserve
	// that diagnostic even though Normalize now trims the whole address.
	// No successful Parse result retains an unnormalized listener.
	if _, port, err := net.SplitHostPort(listen); err == nil && port != trim(port) {
		cfg.API.Listen = listen
	}
	if err := cfg.Validate(); err != nil {
		return nil, redactEnvError(err, values)
	}
	return cfg, nil
}

// parseYAMLDocument retains the original nodes, comments and scalar spelling.
func parseYAMLDocument(data []byte) (*yaml.Node, error) {
	var document yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&document); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("config is empty")
		}
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	var trailing yaml.Node
	if err := dec.Decode(&trailing); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse trailing YAML document: %w", err)
		}
		return nil, fmt.Errorf("config must contain exactly one YAML document")
	}
	return &document, nil
}

// decodeConfig expands and strictly decodes a document, without semantic
// validation. It mutates document; callers needing its raw values must retain
// them before calling. Both parsing paths share decoding and error redaction.
func decodeConfig(document *yaml.Node, path string) (*Config, map[string]struct{}, error) {
	changed, values, err := expandEnvScalars(document)
	if err != nil {
		return nil, nil, redactEnvError(err, values)
	}
	if err := normalizeExpandedBooleans(document, changed); err != nil {
		return nil, nil, redactEnvError(err, values)
	}
	expanded, err := yaml.Marshal(document)
	if err != nil {
		return nil, nil, redactEnvError(fmt.Errorf("encode YAML: %w", err), values)
	}

	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if len(changed) > 0 {
			err = fmt.Errorf("%w; environment-expanded scalars are strings (use YAML literals for numbers; booleans must expand to true or false)", err)
		}
		return nil, nil, redactEnvError(fmt.Errorf("parse YAML: %w", err), values)
	}
	if err := validateExpandedBooleanValues(document, changed); err != nil {
		return nil, nil, redactEnvError(err, values)
	}
	cfg.Path = path
	return cfg, values, nil
}

// Normalize lower-cases enumerated values and fills per-element defaults.
func (c *Config) Normalize() {
	c.Log.Level = lowerTrim(c.Log.Level)
	c.Log.Format = lowerTrim(c.Log.Format)
	c.API.Listen = trim(c.API.Listen)
	c.API.Token = trim(c.API.Token)
	c.State.Path = trim(c.State.Path)
	c.RouterOS.URL = trimRightSlash(trim(c.RouterOS.URL))
	c.RouterOS.Username = trim(c.RouterOS.Username)
	c.RouterOS.TLS.CAFile = trim(c.RouterOS.TLS.CAFile)

	if c.DNS.Providers == nil {
		c.DNS.Providers = map[string]ProviderConfig{}
	}
	for name, p := range c.DNS.Providers {
		p.Type = lowerTrim(p.Type)
		p.Endpoint = trim(p.Endpoint)
		if p.Endpoint == "" {
			p.Endpoint = "alidns.aliyuncs.com"
		}
		p.RegionID = trim(p.RegionID)
		if p.RegionID == "" {
			p.RegionID = "cn-hangzhou"
		}
		if !p.Timeout.set && p.Timeout.Duration == 0 {
			p.Timeout.Duration = defaultProviderTimeout
		}
		if !p.qpsSet && p.QPS == 0 {
			p.QPS = 5
		}
		c.DNS.Providers[name] = p
	}

	if !c.RouterOS.Timeout.set && c.RouterOS.Timeout.Duration == 0 {
		c.RouterOS.Timeout.Duration = defaultRouterOSTimeout
	}

	for i := range c.Items {
		it := &c.Items[i]
		it.Name = trim(it.Name)
		it.Source.Interface = trim(it.Source.Interface)
		it.Source.Family = lowerTrim(it.Source.Family)
		it.Source.EUI64MAC = trim(it.Source.EUI64MAC)
		it.Source.Select.Scope = lowerTrim(it.Source.Select.Scope)
		if it.Source.Select.Scope == "" {
			it.Source.Select.Scope = ScopePublic
		}
		it.Source.Select.Prefix = trim(it.Source.Select.Prefix)

		if it.AddressList != nil {
			it.AddressList.List = trim(it.AddressList.List)
			it.AddressList.Timeout = trim(it.AddressList.Timeout)
			it.AddressList.Comment = trim(it.AddressList.Comment)
			if it.AddressList.Comment == "" {
				it.AddressList.Comment = "ros-ddns:" + it.Name
			}
		}

		for j := range it.Domains {
			d := &it.Domains[j]
			d.Provider = trim(d.Provider)
			d.Domain = trimRightDot(lowerTrim(d.Domain))
			d.RR = lowerTrim(d.RR)
			d.Line = trim(d.Line)
			if d.Line == "" {
				d.Line = "default"
			}
			if !d.ttlSet && d.TTL == 0 {
				d.TTL = 600
			}
		}
	}
}
