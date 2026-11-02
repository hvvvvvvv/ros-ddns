package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ros-ddns/internal/eui64"
	"ros-ddns/internal/pathguard"
)

const (
	defaultProviderTimeout = 10 * time.Second
	defaultRouterOSTimeout = 10 * time.Second
)

var itemNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var fqdnLabelRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Validate performs semantic validation. Call Normalize first (Load does both).
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if err := pathguard.Validate(c.Path, c.State.Path, ""); err != nil {
		add("%s", err)
	}

	// log
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level: unsupported value %q (debug|info|warn|error)", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		add("log.format: unsupported value %q (text|json)", c.Log.Format)
	}

	// runtime
	if c.Runtime.PollInterval.Duration <= 0 {
		add("runtime.pollInterval: must be greater than 0")
	}
	if c.Runtime.StableFor.Duration < 0 {
		add("runtime.stableFor: must not be negative")
	}
	if c.Runtime.ReconcileTimeout.Duration <= 0 {
		add("runtime.reconcileTimeout: must be greater than 0")
	}
	if c.Runtime.VerifyInterval.Duration < 0 {
		add("runtime.verifyInterval: must not be negative")
	}
	if c.Runtime.MaxBackoff.Duration < c.Runtime.PollInterval.Duration {
		add("runtime.maxBackoff: must be greater than or equal to runtime.pollInterval")
	}

	// api
	if c.API.Listen == "" {
		add("api.listen: must not be empty")
	} else if host, port, err := net.SplitHostPort(c.API.Listen); err != nil {
		add("api.listen: invalid address %q: %v", c.API.Listen, err)
	} else if port == "" {
		add("api.listen: port is required")
	} else if !validListenPort(port) {
		add("api.listen: port must be a decimal number between 1 and 65535")
	} else if !loopbackListenHost(host) && c.API.Token == "" {
		add("api.token: a token is required when api.listen binds a non-loopback address (%q); bind to 127.0.0.1/::1 or set a token", c.API.Listen)
	}

	// routeros
	if c.RouterOS.URL == "" {
		add("routeros.url: must not be empty")
	} else if u, err := url.Parse(c.RouterOS.URL); err != nil {
		// Parser errors can echo the raw URL, including userinfo passwords.
		add("routeros.url: invalid URL")
	} else {
		switch {
		case u.Scheme != "http" && u.Scheme != "https":
			add("routeros.url: unsupported scheme %q (http|https)", u.Scheme)
		case u.Hostname() == "":
			add("routeros.url: host is required")
		case u.User != nil:
			add("routeros.url: userinfo is not allowed; use routeros.username and routeros.password")
		case strings.HasSuffix(u.Host, ":") || (u.Port() != "" && !validListenPort(u.Port())):
			// Port alone cannot distinguish an absent port from an explicit empty one.
			add("routeros.url: port must be a decimal number between 1 and 65535")
		case u.RawQuery != "" || u.ForceQuery || strings.Contains(c.RouterOS.URL, "#"):
			// Check the raw delimiter too: url.Parse drops an empty fragment.
			add("routeros.url: query strings and fragments are not allowed")
		case strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/rest"):
			add("routeros.url: do not include /rest; the client appends it automatically")
		}
	}
	if c.RouterOS.Username == "" {
		add("routeros.username: must not be empty")
	}
	if c.RouterOS.Timeout.Duration <= 0 {
		add("routeros.timeout: must be greater than 0")
	}
	if c.RouterOS.TLS.InsecureSkipVerify && c.RouterOS.TLS.CAFile != "" {
		add("routeros.tls: insecureSkipVerify and caFile are mutually exclusive")
	}

	for name, p := range c.DNS.Providers {
		label := "dns.providers." + name
		if p.Type != "aliyun" {
			add("%s.type: unsupported provider type %q (supported: aliyun)", label, p.Type)
		}
		if p.Endpoint == "" {
			add("%s.endpoint: must not be empty", label)
		}
		if p.Timeout.Duration <= 0 {
			add("%s.timeout: must be greater than 0", label)
		}
		if p.QPS < 1 {
			add("%s.qps: must be at least 1", label)
		}
		// Provider credentials are intentionally not required here: a missing or
		// invalid AccessKey must not prevent the daemon from starting. Provider
		// requests fail at synchronization time and are reported in the log and
		// in-app notifications, then retried with backoff.
	}

	if len(c.Items) == 0 {
		add("items: at least one item is required")
	}

	enabledItems := 0
	seenNames := map[string]bool{}

	for i := range c.Items {
		it := &c.Items[i]
		label := fmt.Sprintf("items[%d]", i)
		if it.Name != "" {
			label = fmt.Sprintf("items[%d] (%s)", i, it.Name)
		}
		if !it.Disabled {
			enabledItems++
		}

		// name
		switch {
		case it.Name == "":
			add("%s.name: must not be empty", label)
		case !itemNameRE.MatchString(it.Name):
			add("%s.name: %q must match ^[a-z0-9][a-z0-9_-]{0,63}$", label, it.Name)
		case seenNames[it.Name]:
			add("%s.name: duplicate item name %q", label, it.Name)
		default:
			seenNames[it.Name] = true
		}

		// source
		if it.Source.Interface == "" {
			add("%s.source.interface: must not be empty", label)
		}
		switch it.Source.Family {
		case FamilyIPv4, FamilyIPv6:
		default:
			add("%s.source.family: unsupported value %q (ipv4|ipv6)", label, it.Source.Family)
		}
		switch it.Source.Select.Scope {
		case ScopePublic, ScopeAny:
		default:
			add("%s.source.select.scope: unsupported value %q (public|any)", label, it.Source.Select.Scope)
		}
		if it.Source.Select.Index < 0 {
			add("%s.source.select.index: must not be negative", label)
		}

		it.Source.Select.PrefixNet = nil
		if it.Source.Select.Prefix != "" {
			p, err := netip.ParsePrefix(it.Source.Select.Prefix)
			if err != nil {
				add("%s.source.select.prefix: invalid CIDR %q: %v", label, it.Source.Select.Prefix, err)
			} else {
				masked := p.Masked()
				it.Source.Select.PrefixNet = &masked
				switch it.Source.Family {
				case FamilyIPv4:
					if !p.Addr().Is4() {
						add("%s.source.select.prefix: %s is not an IPv4 prefix", label, p)
					}
				case FamilyIPv6:
					if !p.Addr().Is6() || p.Addr().Is4In6() {
						add("%s.source.select.prefix: %s is not an IPv6 prefix", label, p)
					}
				}
			}
		}

		it.Source.EUI64MACAddr = nil
		if it.Source.EUI64MAC != "" {
			if it.Source.Family != FamilyIPv6 {
				add("%s.source.eui64-mac: only valid for family %s", label, FamilyIPv6)
			}
			hw, err := eui64.ParseMAC(it.Source.EUI64MAC)
			if err != nil {
				add("%s.source.eui64-mac: %v", label, err)
			} else {
				it.Source.EUI64MAC = hw.String()
				it.Source.EUI64MACAddr = hw
			}
		}

		if len(it.Domains) == 0 && it.AddressList == nil {
			add("%s: at least one of domains or addressList is required", label)
		}

		for j := range it.Domains {
			d := &it.Domains[j]
			dlabel := fmt.Sprintf("%s.domains[%d]", label, j)
			if d.Provider == "" {
				add("%s.provider: must not be empty", dlabel)
			} else if _, ok := c.DNS.Providers[d.Provider]; !ok {
				add("%s.provider: unknown provider %q", dlabel, d.Provider)
			}
			if d.Domain == "" {
				add("%s.domain: must not be empty", dlabel)
			} else if !validDomain(d.Domain) {
				add("%s.domain: invalid domain %q", dlabel, d.Domain)
			}
			if d.RR == "" {
				add("%s.rr: must not be empty", dlabel)
			} else if !validRR(d.RR) {
				add("%s.rr: invalid record name %q", dlabel, d.RR)
			}
			if d.TTL < 1 || d.TTL > 86400 {
				add("%s.ttl: must be between 1 and 86400", dlabel)
			}
			if d.Line == "" {
				add("%s.line: must not be empty", dlabel)
			}
			if d.Domain != "" && d.RR != "" && len(d.FQDN()) > 253 {
				add("%s: fully qualified name exceeds 253 characters", dlabel)
			}
		}

		if it.AddressList != nil {
			al := it.AddressList
			if al.List == "" {
				add("%s.addressList.list: must not be empty", label)
			}
			if al.Comment == "" {
				add("%s.addressList.comment: must not be empty", label)
			}
			if al.Timeout != "" {
				if _, err := parseROSDuration(al.Timeout); err != nil {
					add("%s.addressList.timeout: %v", label, err)
				}
			}
		}
	}

	if len(c.Items) > 0 && enabledItems == 0 {
		add("items: at least one item must be enabled")
	}

	// Duplicate targets are only a problem for enabled items.
	recordKeys := map[string]string{}
	listKeys := map[string]string{}
	for i := range c.Items {
		it := &c.Items[i]
		if it.Disabled {
			continue
		}
		typ := RecordType(it.Source.Family)
		for _, d := range it.Domains {
			key := strings.Join([]string{d.Provider, d.Domain, d.RR, typ, d.Line}, "\x00")
			if prev, ok := recordKeys[key]; ok {
				add("items[%d] (%s): duplicate record target %s %s (already configured by item %s)", i, it.Name, d.FQDN(), typ, prev)
			} else {
				recordKeys[key] = it.Name
			}
		}
		if it.AddressList != nil {
			key := strings.Join([]string{it.Source.Family, it.AddressList.List, it.AddressList.Comment}, "\x00")
			if prev, ok := listKeys[key]; ok {
				add("items[%d] (%s): duplicate address-list target list=%q comment=%q (already configured by item %s)",
					i, it.Name, it.AddressList.List, it.AddressList.Comment, prev)
			} else {
				listKeys[key] = it.Name
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid config:\n - %s", strings.Join(problems, "\n - "))
	}
	return nil
}

func validListenPort(port string) bool {
	for _, ch := range port {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

func validDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !fqdnLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

func validRR(rr string) bool {
	if rr == "@" || rr == "*" {
		return true
	}
	if len(rr) == 0 || len(rr) > 253 {
		return false
	}
	for _, label := range strings.Split(rr, ".") {
		if !fqdnLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// loopbackListenHost reports whether binding to host keeps the API local.
// An empty host (e.g. ":8080") binds all interfaces and is not loopback.
func loopbackListenHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if host == "" {
		return false
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}

func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func trimRightSlash(s string) string {
	return strings.TrimRight(s, "/")
}

func trimRightDot(s string) string {
	return strings.TrimSuffix(s, ".")
}
