package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"ros-ddns/internal/config"
	"ros-ddns/internal/dns"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/state"
)

func tolerantConfig() *config.Config {
	c := testConfig()
	c.Normalize()
	return c
}

func tolerantDeps(router ros.Client, provider dns.Provider) Deps {
	return Deps{
		Tolerant:    true,
		NewROS:      func(config.RouterOSConfig) (ros.Client, error) { return router, nil },
		NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) { return provider, nil },
	}
}

func assertInvalidCount(t *testing.T, s *Service, want int) {
	t.Helper()
	count := 0
	for _, status := range s.Snapshot().Items {
		if !status.Disabled && status.Phase == "error" {
			count++
		}
	}
	if s.invalid != want || s.invalid != count {
		t.Fatalf("invalid count = %d, enabled error slots = %d, want %d", s.invalid, count, want)
	}
}

func TestTolerantStartupReportsItemAndProviderProblems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
		want   []string
	}{
		{"missing interface", func(c *config.Config) { c.Items[0].Source.Interface = "" }, []string{"source.interface"}},
		{"unknown provider", func(c *config.Config) { c.Items[0].Domains[0].Provider = "missing" }, []string{`unknown provider "missing"`}},
		{"provider settings", func(c *config.Config) {
			p := c.DNS.Providers["test"]
			p.QPS = -1
			c.DNS.Providers["test"] = p
		}, []string{"dns.providers.test.qps"}},
		{"merged problems", func(c *config.Config) {
			c.Items[0].Source.Interface = ""
			p := c.DNS.Providers["test"]
			p.Type = "unsupported"
			c.DNS.Providers["test"] = p
		}, []string{"source.interface", "dns.providers.test.type"}},
		{"redacted configuration", func(c *config.Config) {
			c.RouterOS.Password = "private-invalid-prefix"
			c.Items[0].Source.Select.Prefix = c.RouterOS.Password
		}, []string{"source.select.prefix", "[redacted]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tolerantConfig()
			tc.change(c)
			deps := Deps{Tolerant: true,
				NewROS: func(config.RouterOSConfig) (ros.Client, error) {
					t.Fatal("constructed RouterOS without runnable items")
					return nil, nil
				},
				NewProvider: func(string, config.ProviderConfig) (dns.Provider, error) {
					t.Fatal("constructed provider for an invalid item")
					return nil, nil
				},
			}
			s, err := New(c, deps)
			if err != nil {
				t.Fatal(err)
			}
			assertInvalidCount(t, s, 1)
			status := s.Snapshot().Items[0]
			if status.Phase != "error" || !strings.HasPrefix(status.LastError, "配置错误：") || s.Ready() || s.TriggerItem("home") {
				t.Fatalf("invalid item was not isolated: %#v", status)
			}
			for _, want := range tc.want {
				if !strings.Contains(status.LastError, want) {
					t.Errorf("error %q does not contain %q", status.LastError, want)
				}
			}
			if c.RouterOS.Password != "" && strings.Contains(status.LastError, c.RouterOS.Password) {
				t.Fatal("configuration error leaked a secret")
			}
		})
	}
}

func TestTolerantStartupPreservesOrderAndRunsHealthyItems(t *testing.T) {
	c := tolerantConfig()
	healthy := config.CloneItem(c.Items[0])
	broken := config.CloneItem(healthy)
	broken.Name, broken.Source.Interface = "broken", ""
	broken.Domains, broken.AddressList = nil, &config.AddressListConfig{List: "broken", Comment: "broken"}
	duplicate := config.CloneItem(healthy)
	unnamed := config.CloneItem(broken)
	unnamed.Name = ""
	disabled := config.ItemConfig{Disabled: true, Domains: []config.DomainConfig{{Provider: "missing"}}}
	c.Items = []config.ItemConfig{disabled, broken, healthy, duplicate, unnamed}
	router, provider := newFakeROS(), &fakeProvider{}
	s, err := New(c, tolerantDeps(router, provider))
	if err != nil {
		t.Fatal(err)
	}
	assertInvalidCount(t, s, 3)
	snapshot, err := s.runOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"item[0]", "broken", "home", "item[3]", "item[4]"}
	wantPhases := []string{"disabled", "error", "synced", "error", "error"}
	if len(snapshot.Items) != len(wantNames) {
		t.Fatalf("lost config positions: %#v", snapshot.Items)
	}
	for i, status := range snapshot.Items {
		if status.Name != wantNames[i] || status.Phase != wantPhases[i] {
			t.Errorf("slot %d = %#v", i, status)
		}
		if i != 2 && s.TriggerItem(status.Name) {
			t.Errorf("trigger accepted non-runnable item %q", status.Name)
		}
	}
	if snapshot.Items[0].LastError != "" || !snapshot.Items[0].Disabled {
		t.Fatal("disabled item was reported as a configuration error")
	}
	if !s.TriggerItem("home") || s.Ready() || len(provider.recorded()) != 2 || len(router.adds) != 1 {
		t.Fatalf("healthy execution or mixed readiness failed: %#v", snapshot.Items)
	}
	snapshot.Items[0].Domains[0].Provider = "mutated"
	snapshot.Items[3].Domains[0].Provider = "mutated"
	fresh := s.Snapshot()
	if fresh.Items[0].Domains[0].Provider == "mutated" || fresh.Items[3].Domains[0].Provider == "mutated" {
		t.Fatal("static status snapshots share mutable slices")
	}
}

func TestTolerantDisabledItemsDoNotPreventReadiness(t *testing.T) {
	c := tolerantConfig()
	c.Items = []config.ItemConfig{{Name: "off", Disabled: true}}
	s, err := New(c, Deps{Tolerant: true})
	if err != nil || !s.Ready() || s.TriggerItem("off") {
		t.Fatalf("disabled-only service: service=%#v error=%v", s, err)
	}
	assertInvalidCount(t, s, 0)
	if status := s.Snapshot().Items[0]; status.Phase != "disabled" || status.LastError != "" {
		t.Fatalf("disabled item failed validation: %#v", status)
	}
}

func TestTolerantProviderFactoryFailuresAreCached(t *testing.T) {
	for _, factoryError := range []bool{true, false} {
		name := "nil provider"
		if factoryError {
			name = "factory error"
		}
		t.Run(name, func(t *testing.T) {
			c := tolerantConfig()
			p := c.DNS.Providers["test"]
			p.AccessKeySecret = "private-provider-credential"
			c.DNS.Providers["test"] = p
			c.DNS.Providers["healthy"] = p
			second := config.CloneItem(c.Items[0])
			second.Name, second.AddressList = "second", nil
			healthy := config.CloneItem(second)
			healthy.Name = "healthy"
			for i := range healthy.Domains {
				healthy.Domains[i].Provider = "healthy"
			}
			c.Items = append(c.Items, second, healthy)
			calls := make(map[string]int)
			provider := &fakeProvider{}
			deps := tolerantDeps(newFakeROS(), provider)
			deps.NewProvider = func(name string, _ config.ProviderConfig) (dns.Provider, error) {
				calls[name]++
				if name == "test" {
					if factoryError {
						return nil, errors.New("cannot create provider with private-provider-credential")
					}
					return nil, nil
				}
				return provider, nil
			}
			s, err := New(c, deps)
			if err != nil {
				t.Fatal(err)
			}
			assertInvalidCount(t, s, 2)
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, status := range snapshot.Items[:2] {
				if status.Phase != "error" || !strings.Contains(status.LastError, "create provider test:") || s.TriggerItem(status.Name) {
					t.Errorf("provider failure did not isolate referencing item: %#v", status)
				}
				if strings.Contains(status.LastError, p.AccessKeySecret) || factoryError && !strings.Contains(status.LastError, "[redacted]") {
					t.Fatal("provider construction error was not redacted")
				}
			}
			if calls["test"] != 1 || calls["healthy"] != 1 || snapshot.Items[2].Phase != "synced" || len(provider.recorded()) != 2 || s.Ready() {
				t.Fatalf("factory failures were not isolated/cached: calls=%v items=%#v", calls, snapshot.Items)
			}
		})
	}
}

func TestTolerantMissingProviderFactoryIsAnItemError(t *testing.T) {
	s, err := New(tolerantConfig(), Deps{Tolerant: true})
	if err != nil {
		t.Fatal(err)
	}
	assertInvalidCount(t, s, 1)
	if status := s.Snapshot().Items[0]; status.Phase != "error" || !strings.Contains(status.LastError, "Deps.NewProvider is required") {
		t.Fatalf("missing provider dependency escaped isolation: %#v", status)
	}
}

func TestTolerantTargetConflictsOnlyRunUncontestedItems(t *testing.T) {
	for _, target := range []string{"dns", "address-list", "within item"} {
		t.Run(target, func(t *testing.T) {
			c := tolerantConfig()
			c.Items[0].Domains = c.Items[0].Domains[:1]
			if target == "address-list" {
				c.Items[0].Domains = nil
			} else {
				c.Items[0].AddressList = nil
			}
			wantConflicts := 1
			if target == "within item" {
				c.Items[0].Domains = append(c.Items[0].Domains, c.Items[0].Domains[0], c.Items[0].Domains[0])
				wantConflicts = 2
			} else {
				second := config.CloneItem(c.Items[0])
				second.Name = "duplicate"
				c.Items = append(c.Items, second)
			}
			conflicts := c.TargetConflicts()
			if len(conflicts) != wantConflicts {
				t.Fatalf("test config has unexpected conflicts: %v", conflicts)
			}
			router, provider := newFakeROS(), &fakeProvider{}
			deps := tolerantDeps(router, provider)
			factoryCalls := 0
			deps.NewROS = func(config.RouterOSConfig) (ros.Client, error) {
				factoryCalls++
				return router, nil
			}
			s, err := New(c, deps)
			if err != nil {
				t.Fatal(err)
			}
			assertInvalidCount(t, s, 1)
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			conflict := conflicts[0]
			status := snapshot.Items[conflict.ItemIndex]
			if status.Phase != "error" || status.LastError != conflict.Message || s.TriggerItem(status.Name) || s.Ready() {
				t.Fatalf("conflict was not retained in its original slot: %#v", status)
			}
			if target == "within item" {
				if factoryCalls != 0 || len(provider.recorded()) != 0 {
					t.Fatal("conflicting-only config constructed or executed RouterOS")
				}
			} else if factoryCalls != 1 || snapshot.Items[0].Phase != "synced" {
				t.Fatalf("uncontested item did not run: %#v", snapshot.Items)
			}
			if target == "dns" && len(provider.recorded()) != 1 || target == "address-list" && len(router.adds) != 1 {
				t.Fatal("conflicting item executed a target")
			}
		})
	}
}

func TestTolerantRouterOSSetupFailuresKeepPolling(t *testing.T) {
	for _, mode := range []string{"incomplete config", "factory error", "nil client"} {
		t.Run(mode, func(t *testing.T) {
			c := tolerantConfig()
			c.RouterOS.Password = "private-router-credential"
			c.Runtime.PollInterval.Duration = 5 * time.Millisecond
			c.Runtime.MaxBackoff.Duration = 20 * time.Millisecond
			if mode == "incomplete config" {
				c.RouterOS.URL, c.RouterOS.Username = "", ""
			}
			factoryCalls := 0
			provider := &fakeProvider{}
			deps := tolerantDeps(nil, provider)
			deps.NewROS = func(config.RouterOSConfig) (ros.Client, error) {
				factoryCalls++
				if mode == "factory error" {
					return nil, errors.New("cannot load CA with private-router-credential")
				}
				return nil, nil
			}
			s, err := New(c, deps)
			if err != nil {
				t.Fatal(err)
			}
			assertInvalidCount(t, s, 0)
			if mode == "incomplete config" && factoryCalls != 0 || mode != "incomplete config" && factoryCalls != 1 {
				t.Fatalf("unexpected RouterOS factory calls: %d", factoryCalls)
			}
			_, addressErr := s.client.Addresses(context.Background(), ros.IPv4)
			_, listErr := s.client.ListEntries(context.Background(), ros.IPv4, "ddns")
			for _, err := range []error{addressErr, listErr,
				s.client.AddEntry(context.Background(), ros.IPv4, ros.ListEntry{}),
				s.client.RemoveEntry(context.Background(), ros.IPv4, "*1")} {
				if err == nil || strings.Contains(err.Error(), c.RouterOS.Password) {
					t.Fatalf("fallback method lost or leaked initialization failure: %v", err)
				}
			}
			cancel, done := startService(t, s)
			defer stopService(t, cancel, done)
			awaitCondition(t, func() bool { return s.Snapshot().Items[0].Phase == "degraded" })
			first := s.Snapshot().Items[0]
			awaitCondition(t, func() bool { return s.Snapshot().Items[0].NextPollAt.After(first.NextPollAt) })
			status := s.Snapshot().Items[0]
			if status.Phase != "degraded" || status.LastError == "" || s.Ready() || len(provider.recorded()) != 0 {
				t.Fatalf("setup failure did not stay in polling path: %#v", status)
			}
			if mode == "incomplete config" && (!strings.Contains(status.LastError, "routeros.url") || !strings.Contains(status.LastError, "routeros.username")) {
				t.Fatalf("incomplete RouterOS diagnostics were lost: %s", status.LastError)
			}
			if mode == "factory error" && !strings.Contains(status.LastError, "[redacted]") {
				t.Fatal("RouterOS setup error was not redacted")
			}
		})
	}
}

func TestTolerantStateOpenFailureFallsBackToMemory(t *testing.T) {
	c := tolerantConfig()
	c.Items = nil
	c.State.Path = t.TempDir() // An existing directory cannot be opened as a cache file.
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	s, err := New(c, Deps{Tolerant: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.Put("memory", state.Item{DesiredValue: "203.0.113.5"}); err != nil {
		t.Fatalf("fallback still attempted disk persistence: %v", err)
	}
	if cached, ok := s.store.Get("memory"); !ok || cached.DesiredValue != "203.0.113.5" {
		t.Fatal("fallback did not retain memory state")
	}
	if !strings.Contains(logs.String(), "using an in-memory cache") {
		t.Fatal("state fallback was not warned about")
	}
	if _, err := New(c, Deps{}); err == nil || !strings.HasPrefix(err.Error(), "open service state: read state:") {
		t.Fatalf("strict state failure changed: %v", err)
	}
}

func TestStrictStartupErrorsRemainUnchanged(t *testing.T) {
	deps := tolerantDeps(newFakeROS(), &fakeProvider{})
	deps.Tolerant = false
	s, err := New(testConfig(), deps)
	if err != nil {
		t.Fatal(err)
	}
	assertInvalidCount(t, s, 0)

	for _, tc := range []struct {
		name   string
		change func(*config.Config, *Deps)
		want   string
	}{
		{"runtime", func(c *config.Config, _ *Deps) { c.Runtime.PollInterval.Duration = 0 }, "invalid runtime intervals; load and validate the config before creating the service"},
		{"empty name", func(c *config.Config, _ *Deps) { c.Items[0].Name = "" }, `empty or duplicate item name ""`},
		{"duplicate name", func(c *config.Config, _ *Deps) { c.Items = append(c.Items, c.Items[0]) }, `empty or duplicate item name "home"`},
		{"family", func(c *config.Config, _ *Deps) { c.Items[0].Source.Family = "invalid" }, `item home: invalid address family "invalid"`},
		{"unknown provider", func(c *config.Config, _ *Deps) { c.Items[0].Domains[0].Provider = "missing" }, `item home: unknown provider "missing"`},
		{"missing provider factory", func(_ *config.Config, d *Deps) { d.NewProvider = nil }, "provider test: Deps.NewProvider is required"},
		{"provider error", func(_ *config.Config, d *Deps) {
			d.NewProvider = func(string, config.ProviderConfig) (dns.Provider, error) { return nil, errors.New("failure") }
		}, "create provider test: failure"},
		{"nil provider", func(_ *config.Config, d *Deps) {
			d.NewProvider = func(string, config.ProviderConfig) (dns.Provider, error) { return nil, nil }
		}, "create provider test: factory returned nil"},
		{"RouterOS error", func(_ *config.Config, d *Deps) {
			d.NewROS = func(config.RouterOSConfig) (ros.Client, error) { return nil, errors.New("failure") }
		}, "create RouterOS client: failure"},
		{"nil RouterOS", func(_ *config.Config, d *Deps) {
			d.NewROS = func(config.RouterOSConfig) (ros.Client, error) { return nil, nil }
		}, "create RouterOS client: factory returned nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			deps := tolerantDeps(newFakeROS(), &fakeProvider{})
			deps.Tolerant = false
			tc.change(c, &deps)
			if _, err := New(c, deps); err == nil || err.Error() != tc.want {
				t.Fatalf("strict New error = %v, want %q", err, tc.want)
			}
		})
	}
}
