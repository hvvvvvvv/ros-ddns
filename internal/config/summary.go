package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Summary renders a human readable, secret-free overview of the config. It is
// used by "ros-ddns -check".
func Summary(c *Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "config: %s\n", c.Path)
	fmt.Fprintf(&b, "routeros: %s (user=%s, password=%s, tls.insecureSkipVerify=%t)\n",
		redactURL(c.RouterOS.URL), c.RouterOS.Username, setState(c.RouterOS.Password), c.RouterOS.TLS.InsecureSkipVerify)
	fmt.Fprintf(&b, "runtime: pollInterval=%s stableFor=%s verifyInterval=%s syncOnStart=%t\n",
		c.Runtime.PollInterval.Duration, c.Runtime.StableFor.Duration, c.Runtime.VerifyInterval.Duration, c.Runtime.SyncOnStart)
	fmt.Fprintf(&b, "api: listen=%s token=%s\n", c.API.Listen, setState(c.API.Token))

	b.WriteString("providers:\n")
	names := make([]string, 0, len(c.DNS.Providers))
	for name := range c.DNS.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, name := range names {
		p := c.DNS.Providers[name]
		fmt.Fprintf(&b, "  %s: type=%s endpoint=%s accessKeyId=%s accessKeySecret=%s securityToken=%s qps=%d\n",
			name, p.Type, p.Endpoint, setState(p.AccessKeyID), setState(p.AccessKeySecret), setState(p.SecurityToken), p.QPS)
	}

	b.WriteString("items:\n")
	for i := range c.Items {
		it := &c.Items[i]
		eui := "-"
		if it.Source.EUI64MAC != "" {
			eui = it.Source.EUI64MAC
		}
		fmt.Fprintf(&b, "  - %s: family=%s type=%s interface=%s scope=%s index=%d eui64-mac=%s disabled=%t\n",
			it.Name, it.Source.Family, RecordType(it.Source.Family), it.Source.Interface,
			it.Source.Select.Scope, it.Source.Select.Index, eui, it.Disabled)
		for _, d := range it.Domains {
			fmt.Fprintf(&b, "      record: %s %s provider=%s ttl=%d line=%s\n",
				d.FQDN(), RecordType(it.Source.Family), d.Provider, d.TTL, d.Line)
		}
		if it.AddressList != nil {
			list := it.AddressList.List
			timeout := it.AddressList.Timeout
			if timeout == "" {
				timeout = "static"
			}
			fmt.Fprintf(&b, "      addressList: %s timeout=%s comment=%s\n", list, timeout, it.AddressList.Comment)
		}
	}
	return b.String()
}

func setState(secret string) string {
	if secret == "" {
		return "unset"
	}
	return "set"
}

// redactURL removes userinfo, queries and fragments, including on malformed
// URLs. Validation rejects these components as well.
func redactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if end := strings.IndexAny(raw, "?#"); end >= 0 {
		raw = raw[:end]
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Malformed authorities can hide credentials in Opaque, Path or parser
		// errors, so do not include any part of an unparseable URL.
		return "[invalid URL]"
	}
	if u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
