package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestValidAPITokenBytes(t *testing.T) {
	for _, token := range []string{"", "!", "~", "ordinary-token", strings.Repeat("x", 256)} {
		if !validAPIToken(token) {
			t.Errorf("rejected valid token of length %d", len(token))
		}
	}
	for _, token := range []string{" ", "a b", "a\tb", "a\nb", "a\rb", "a\x00b", "a\x1fb", "a\x7fb", "a\x80b", "中文", strings.Repeat("x", 257)} {
		if validAPIToken(token) {
			t.Errorf("accepted invalid token of length %d", len(token))
		}
	}
	for b := 0; b <= 255; b++ {
		if got, want := validAPIToken(string([]byte{byte(b)})), b >= 0x21 && b <= 0x7e; got != want {
			t.Errorf("token byte %#x: got %t, want %t", b, got, want)
		}
	}
}

func TestValidateAPITokenRules(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8080", "0.0.0.0:8080"} {
		for _, token := range []string{"valid-token", strings.Repeat("x", 256), "invalid token", "bad\tvalue", "中文", strings.Repeat("x", 257)} {
			t.Run(fmt.Sprintf("%s/%d/%t", listen, len(token), validAPIToken(token)), func(t *testing.T) {
				raw := fmt.Sprintf("api: {listen: %q, token: %q}\n", listen, token) + minimalConfig
				_, err := Parse([]byte(raw), "")
				if validAPIToken(token) {
					if err != nil {
						t.Fatalf("valid token rejected: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "api.token:") || strings.Contains(err.Error(), token) {
					t.Fatalf("expected safe token validation error, got %v", err)
				}
			})
		}
	}
	cfg := mustStartup(t, minimalConfig).Config
	cfg.API.Listen, cfg.API.Token = "0.0.0.0:8080", ""
	const want = "invalid config:\n - api.token: a token is required when api.listen binds a non-loopback address (\"0.0.0.0:8080\"); bind to 127.0.0.1/::1 or set a token"
	if err := cfg.Validate(); err == nil || err.Error() != want {
		t.Fatalf("missing token message changed: %v", err)
	}
	cfg.API.Listen = "127.0.0.1:8080"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty local token must remain valid: %v", err)
	}
}

func TestListenNormalizationPreservesStrictPortRejection(t *testing.T) {
	for _, suffix := range []string{" ", "\t", "\n"} {
		listen := "127.0.0.1:8080" + suffix
		cfg := Default()
		cfg.API.Listen = listen
		cfg.Normalize()
		if cfg.API.Listen != "127.0.0.1:8080" {
			t.Fatal("Normalize must trim api.listen")
		}
		raw := fmt.Sprintf("api: {listen: %q}\n", listen) + minimalConfig
		if _, err := Parse([]byte(raw), ""); err == nil || !strings.Contains(err.Error(), "api.listen: port must be") {
			t.Fatalf("strict Parse must retain its port whitespace rejection: %v", err)
		}
		startup := mustStartup(t, raw)
		if startup.Config.API.Listen != cfg.API.Listen || len(startup.Fixups) != 0 {
			t.Fatal("startup must use the normalized listener without a fixup")
		}
	}
}

func TestCheckRouterOSAndProviderMessages(t *testing.T) {
	cfg := Default()
	cfg.RouterOS = RouterOSConfig{TLS: TLSConfig{InsecureSkipVerify: true, CAFile: "test.pem"}}
	wantRouter := []string{
		"routeros.url: must not be empty",
		"routeros.username: must not be empty",
		"routeros.timeout: must be greater than 0",
		"routeros.tls: insecureSkipVerify and caFile are mutually exclusive",
	}
	if got := cfg.CheckRouterOS(); !reflect.DeepEqual(got, wantRouter) {
		t.Fatalf("router problems = %#v, want %#v", got, wantRouter)
	}
	cfg.DNS.Providers["test"] = ProviderConfig{Type: "unsupported"}
	wantProvider := []string{
		`dns.providers.test.type: unsupported provider type "unsupported" (supported: aliyun)`,
		"dns.providers.test.endpoint: must not be empty",
		"dns.providers.test.timeout: must be greater than 0",
		"dns.providers.test.qps: must be at least 1",
	}
	if got := cfg.CheckProvider("test"); !reflect.DeepEqual(got, wantProvider) {
		t.Fatalf("provider problems = %#v, want %#v", got, wantProvider)
	}
	if got := cfg.CheckProvider("absent"); got != nil {
		t.Errorf("unknown provider problems = %#v, want nil", got)
	}
	valid := mustStartup(t, providerConfig).Config
	if valid.CheckRouterOS() != nil || valid.CheckProvider("ali-main") != nil {
		t.Fatal("valid optional settings must have no problems")
	}
}

func TestCheckItemMessagesAndDisabledItems(t *testing.T) {
	cfg := Default()
	cfg.Items = []ItemConfig{
		{Name: "same"},
		{
			Name: "same", Disabled: true,
			Source: SourceConfig{Family: "other", Select: SelectConfig{Scope: "other", Index: -1}},
			Domains: []DomainConfig{
				{},
				{Provider: "missing", Domain: "bad_domain", RR: "bad*", TTL: 86401, Line: "default"},
			},
			AddressList: &AddressListConfig{},
		},
	}
	want := []string{
		`items[1] (same).name: duplicate item name "same"`,
		"items[1] (same).source.interface: must not be empty",
		`items[1] (same).source.family: unsupported value "other" (ipv4|ipv6)`,
		`items[1] (same).source.select.scope: unsupported value "other" (public|any)`,
		"items[1] (same).source.select.index: must not be negative",
		"items[1] (same).domains[0].provider: must not be empty",
		"items[1] (same).domains[0].domain: must not be empty",
		"items[1] (same).domains[0].rr: must not be empty",
		"items[1] (same).domains[0].ttl: must be between 1 and 86400",
		"items[1] (same).domains[0].line: must not be empty",
		`items[1] (same).domains[1].provider: unknown provider "missing"`,
		`items[1] (same).domains[1].domain: invalid domain "bad_domain"`,
		`items[1] (same).domains[1].rr: invalid record name "bad*"`,
		"items[1] (same).domains[1].ttl: must be between 1 and 86400",
		"items[1] (same).addressList.list: must not be empty",
		"items[1] (same).addressList.comment: must not be empty",
	}
	for n := 0; n < 2; n++ {
		if got := cfg.CheckItem(1); !reflect.DeepEqual(got, want) {
			t.Fatalf("item problems = %#v, want %#v", got, want)
		}
	}
	for _, index := range []int{-1, len(cfg.Items), len(cfg.Items) + 10} {
		if got := cfg.CheckItem(index); got != nil {
			t.Errorf("out-of-range index %d returned problems: %#v", index, got)
		}
	}
	cfg.Items[1].Name = "Invalid"
	if got := cfg.CheckItem(1)[0]; got != `items[1] (Invalid).name: "Invalid" must match ^[a-z0-9][a-z0-9_-]{0,63}$` {
		t.Errorf("invalid name message = %q", got)
	}
	cfg.Items[1].Name = ""
	if got := cfg.CheckItem(1)[0]; got != "items[1].name: must not be empty" {
		t.Errorf("empty name message = %q", got)
	}
}

func TestCheckItemRefreshesDerivedFields(t *testing.T) {
	raw := strings.Replace(minimalConfig, "family: ipv6", "family: ipv6\n      eui64-mac: 001A2B3C4D5E\n      select: {prefix: '2001:db8::123/64'}", 1)
	cfg := mustStartup(t, raw).Config
	source := &cfg.Items[0].Source
	if source.Select.PrefixNet != nil || source.EUI64MACAddr != nil {
		t.Fatal("startup path must leave item semantics and derived fields to CheckItem")
	}
	for n := 0; n < 2; n++ {
		if problems := cfg.CheckItem(0); len(problems) != 0 {
			t.Fatalf("valid item problems: %v", problems)
		}
		if source.Select.PrefixNet == nil || source.Select.PrefixNet.String() != "2001:db8::/64" || source.EUI64MAC != "00:1a:2b:3c:4d:5e" || source.EUI64MACAddr.String() != "00:1a:2b:3c:4d:5e" {
			t.Fatal("derived fields missing or not canonical")
		}
	}
	source.Select.Prefix, source.EUI64MAC = "bad-prefix", "bad-mac"
	if len(cfg.CheckItem(0)) != 2 || source.Select.PrefixNet != nil || source.EUI64MACAddr != nil {
		t.Fatal("invalid replacement values retained stale derived fields")
	}
	source.Select.Prefix, source.EUI64MAC = "", ""
	if problems := cfg.CheckItem(0); len(problems) != 0 || source.Select.PrefixNet != nil || source.EUI64MACAddr != nil {
		t.Fatal("removed values retained stale derived fields")
	}
}

func TestTargetConflictsAndStrictMessageOrder(t *testing.T) {
	cfg := Default()
	cfg.State.Path = ""
	cfg.Log.Level = "invalid"
	cfg.DNS.Providers["ali"] = ProviderConfig{Type: "unsupported", Endpoint: "endpoint", Timeout: Duration{Duration: defaultProviderTimeout}, QPS: 5}
	for _, entry := range []struct {
		name     string
		disabled bool
	}{
		{"disabled", true}, {"same", false}, {"same", false}, {"later", false},
	} {
		cfg.Items = append(cfg.Items, ItemConfig{
			Name: entry.name, Disabled: entry.disabled,
			Source:      SourceConfig{Interface: "wan", Family: FamilyIPv4, Select: SelectConfig{Scope: ScopePublic}},
			Domains:     []DomainConfig{{Provider: "ali", Domain: "example.com", RR: "home", TTL: 600, Line: "default"}},
			AddressList: &AddressListConfig{List: "list", Comment: "shared"},
		})
	}
	// Existing strict behavior also rejects a record repeated inside one item.
	cfg.Items[1].Domains = append(cfg.Items[1].Domains, cfg.Items[1].Domains[0])
	want := []TargetConflict{
		{1, "items[1] (same): duplicate record target home.example.com A (already configured by item same)"},
		{2, "items[2] (same): duplicate record target home.example.com A (already configured by item same)"},
		{2, `items[2] (same): duplicate address-list target list="list" comment="shared" (already configured by item same)`},
		{3, "items[3] (later): duplicate record target home.example.com A (already configured by item same)"},
		{3, `items[3] (later): duplicate address-list target list="list" comment="shared" (already configured by item same)`},
	}
	if got := cfg.TargetConflicts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("conflicts = %#v, want %#v", got, want)
	}
	wantMessages := []string{
		`log.level: unsupported value "invalid" (debug|info|warn|error)`,
		"routeros.url: must not be empty",
		"routeros.username: must not be empty",
		`dns.providers.ali.type: unsupported provider type "unsupported" (supported: aliyun)`,
		`items[2] (same).name: duplicate item name "same"`,
	}
	for _, conflict := range want {
		wantMessages = append(wantMessages, conflict.Message)
	}
	wantError := "invalid config:\n - " + strings.Join(wantMessages, "\n - ")
	if err := cfg.Validate(); err == nil || err.Error() != wantError {
		t.Fatalf("strict error = %v, want %s", err, wantError)
	}
	for i := range cfg.Items {
		cfg.Items[i].Disabled = true
	}
	if got := cfg.TargetConflicts(); got != nil {
		t.Errorf("disabled targets must not conflict: %v", got)
	}
	if len(cfg.CheckItem(2)) != 1 {
		t.Fatal("disabled duplicate item names must still be validated")
	}
}
