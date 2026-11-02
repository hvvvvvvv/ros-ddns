package service

import (
	"net"
	"net/netip"
	"testing"
)

func TestNewOwnsItemConfiguration(t *testing.T) {
	cfg := testConfig()
	prefix := netip.MustParsePrefix("203.0.113.0/24")
	cfg.Items[0].Source.Select.PrefixNet = &prefix
	cfg.Items[0].Source.EUI64MACAddr = net.HardwareAddr{0, 0x11, 0x22, 0x33, 0x44, 0x55}
	svc, _ := testService(t, cfg, newFakeROS(), &fakeProvider{})

	cfg.Items[0].Name = "changed"
	cfg.Items[0].Domains[0].Domain = "changed.example"
	cfg.Items[0].Source.EUI64MACAddr[0] = 0xff
	*cfg.Items[0].Source.Select.PrefixNet = netip.MustParsePrefix("198.51.100.0/24")
	cfg.Items[0].AddressList.List = "changed"
	got := svc.items[0].cfg
	if got.Name != "home" || got.Domains[0].Domain != "example.com" ||
		got.Source.EUI64MACAddr[0] != 0 || got.Source.Select.PrefixNet.String() != "203.0.113.0/24" ||
		got.AddressList.List != "ddns" {
		t.Fatal("service retained mutable caller-owned item configuration")
	}
}
