package app

import (
	"net"
	"net/netip"
	"reflect"
	"testing"

	"ros-ddns/internal/config"
)

func TestCloneConfigOwnership(t *testing.T) {
	if cloneConfig(nil) != nil {
		t.Fatal("nil config must remain nil")
	}
	cfg := config.Default()
	cfg.DNS.Providers["test"] = config.ProviderConfig{Type: "aliyun"}
	prefix := netip.MustParsePrefix("2001:db8::/64")
	cfg.Items = []config.ItemConfig{{
		Name: "home",
		Source: config.SourceConfig{
			EUI64MACAddr: net.HardwareAddr{0, 0x11, 0x22, 0x33, 0x44, 0x55},
			Select:       config.SelectConfig{PrefixNet: &prefix},
		},
		Domains:     []config.DomainConfig{{Domain: "example.com"}},
		AddressList: &config.AddressListConfig{List: "ddns"},
	}}
	cloned := cloneConfig(cfg)
	if !reflect.DeepEqual(cloned, cfg) {
		t.Fatal("configuration values changed while cloning")
	}
	cloned.API.Token = "changed"
	delete(cloned.DNS.Providers, "test")
	cloned.Items[0].Name = "changed"
	cloned.Items[0].Domains[0].Domain = "changed.example"
	cloned.Items[0].Source.EUI64MACAddr[0] = 0xff
	*cloned.Items[0].Source.Select.PrefixNet = netip.MustParsePrefix("2001:db8:1::/64")
	cloned.Items[0].AddressList.List = "changed"
	if cfg.API.Token == "changed" || cfg.DNS.Providers["test"].Type != "aliyun" ||
		cfg.Items[0].Name != "home" || cfg.Items[0].Domains[0].Domain != "example.com" ||
		cfg.Items[0].Source.EUI64MACAddr[0] != 0 || cfg.Items[0].Source.Select.PrefixNet.String() != "2001:db8::/64" ||
		cfg.Items[0].AddressList.List != "ddns" {
		t.Fatal("cloned configuration shares caller-owned data")
	}
}
