package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ros-ddns/internal/config"
	"ros-ddns/internal/state"
)

func TestConfiguredSecretsAreRedactedInLogsStatusAndState(t *testing.T) {
	// passw0d is seven bytes; also cover the eight-byte passw0rd example.
	for _, password := range []string{"p", "passw0d", "passw0rd", "long-router-password"} {
		t.Run(password, func(t *testing.T) {
			c := testConfig()
			c.RouterOS.Password = password
			c.RouterOS.URL = "https://user:url1234@router.example"
			c.API.Token = "api1234"
			c.DNS.Providers["test"] = config.ProviderConfig{AccessKeyID: "key1234", AccessKeySecret: "sec1234", SecurityToken: "tok1234"}
			c.State.Path = filepath.Join(t.TempDir(), "state.json")
			message := `poll ip http password=` + password + ` "` + password + `" api=api1234 ak=key1234 secret=sec1234 token=tok1234 url=url1234`
			want := `poll ip http password=[redacted] "[redacted]" api=[redacted] ak=[redacted] secret=[redacted] token=[redacted] url=[redacted]`
			provider := &fakeProvider{errorText: message}
			provider.fail("home.example.com", 1)
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previousLogger)
			s, _ := testService(t, c, newFakeROS(), provider)
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := snapshot.Items[0].Domains[0].LastError; got != want || !strings.Contains(snapshot.Items[0].LastError, want) {
				t.Fatalf("status error was leaked or corrupted: %q", got)
			}
			data, err := os.ReadFile(c.State.Path)
			if err != nil {
				t.Fatal(err)
			}
			statusJSON, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			for name, text := range map[string]string{"logs": logs.String(), "status": string(statusJSON), "state": string(data)} {
				if !strings.Contains(text, "poll ip http password=[redacted]") {
					t.Errorf("%s lost redacted error: %s", name, text)
				}
				for _, secret := range []string{"password=" + password, `\"` + password + `\"`, "api1234", "key1234", "sec1234", "tok1234", "url1234"} {
					if strings.Contains(text, secret) {
						t.Errorf("%s leaked %q: %s", name, secret, text)
					}
				}
			}
		})
	}
}

func TestEscapedProviderErrorIsRedactedInLogsSnapshotAndPersistedState(t *testing.T) {
	const secret = "credential\"with\\slash\nand newline"
	encoded, _ := json.Marshal(secret)
	for name, representation := range map[string]string{
		"quoted": fmt.Sprintf("%q", secret),
		"JSON":   string(encoded),
		"query":  url.QueryEscape(secret),
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			c.DNS.Providers["test"] = config.ProviderConfig{AccessKeySecret: secret}
			c.State.Path = filepath.Join(t.TempDir(), "state.json")
			message := "provider credential=" + representation
			want := "provider credential=[redacted]"
			if name != "query" {
				want = `provider credential="[redacted]"`
			}
			provider := &fakeProvider{errorText: message}
			provider.fail("home.example.com", 1)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			s, _ := testService(t, c, newFakeROS(), provider)
			if got := s.errorText(errors.New(message)); got != want {
				t.Fatalf("errorText = %q, want %q", got, want)
			}
			snapshot, err := s.runOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := snapshot.Items[0]; got.Domains[0].LastError != want || !strings.Contains(got.LastError, want) {
				t.Fatalf("escaped credential leaked into snapshot: %#v", got)
			}
			persisted, err := state.Open(c.State.Path)
			if err != nil {
				t.Fatal(err)
			}
			cached, ok := persisted.Get("home")
			if !ok || cached.Domains[bindingKey("ipv4", c.Items[0].Domains[0])].LastError != want {
				t.Fatalf("escaped credential leaked into persisted state: %#v", cached)
			}
			decoder := json.NewDecoder(&logs)
			found := false
			for decoder.More() {
				var entry map[string]any
				if err := decoder.Decode(&entry); err != nil {
					t.Fatal(err)
				}
				if entry["msg"] == "DNS reconciliation failed" {
					found = true
					if entry["error"] != want {
						t.Fatalf("escaped credential leaked into log: %#v", entry)
					}
				}
			}
			if !found {
				t.Fatal("missing provider failure log")
			}
		})
	}
}
