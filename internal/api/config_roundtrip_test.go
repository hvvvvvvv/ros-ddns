package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ros-ddns/internal/app"
	"ros-ddns/internal/config"
	"ros-ddns/internal/ros"
	"ros-ddns/internal/service"
)

// Any worker started during these dormant config transactions is a test failure.
type configOnlyROS struct{ t *testing.T }

func (r configOnlyROS) Addresses(context.Context, ros.Family) ([]ros.Address, error) {
	r.t.Error("configuration transaction started a RouterOS poll")
	return nil, fmt.Errorf("unexpected poll")
}
func (r configOnlyROS) ListEntries(context.Context, ros.Family, string) ([]ros.ListEntry, error) {
	r.t.Error("configuration transaction read RouterOS list")
	return nil, fmt.Errorf("unexpected list read")
}
func (r configOnlyROS) AddEntry(context.Context, ros.Family, ros.ListEntry) error {
	r.t.Error("configuration transaction wrote RouterOS list")
	return fmt.Errorf("unexpected list write")
}
func (r configOnlyROS) RemoveEntry(context.Context, ros.Family, string) error {
	r.t.Error("configuration transaction deleted RouterOS entry")
	return fmt.Errorf("unexpected removal")
}

func configRoundTripManager(t *testing.T, apiYAML string) (*app.Manager, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	path, lockPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "instance.lock")
	raw := []byte(apiYAML + `
state: {path: ""}
routeros: {url: "https://router.invalid", username: test}
items:
  - name: home
    source: {interface: wan, family: ipv4}
    addressList: {list: home}
`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("lock sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := app.NewManager(cfg, service.Deps{NewROS: func(config.RouterOSConfig) (ros.Client, error) { return configOnlyROS{t}, nil }}, app.Options{LockPath: lockPath})
	if err != nil {
		t.Fatal(err)
	}
	return m, lockPath, raw
}

func TestMergedTokenManagerGETPUTRoundTrip(t *testing.T) {
	for name, yaml := range map[string]string{
		"inline": `api: {<<: {token: "${TEST_TOKEN}"}}`,
		"merge alias": `<<: {api: &defaults {token: "${TEST_TOKEN}"}}
api: {<<: *defaults}`,
		"merge sequence": `api: {<<: [{token: "${TEST_TOKEN}"}, {token: 123}]}`,
		"nested alias": `<<: {api: &defaults {token: "${TEST_TOKEN}"}}
api: {<<: [{<<: *defaults}, {token: 123}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			const secret = "expanded-token-must-never-be-saved"
			t.Setenv("TEST_TOKEN", secret)
			m, _, original := configRoundTripManager(t, yaml)
			doc, err := m.ConfigDocument()
			if err != nil || !doc.APITokenString || doc.APITokenValue != secret || doc.Data["api"].(map[string]any)["token"] != "${TEST_TOKEN}" {
				t.Fatal("real document did not retain token tag/reference and effective value")
			}
			handler := NewHandler(Options{Config: m, Token: secret})
			send := func(method, path, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+secret)
				if body != "" {
					r.Header.Set("Content-Type", "application/json")
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w
			}
			w := send(http.MethodGet, "/api/v1/config", "")
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), secret) {
				t.Fatalf("unsafe GET: %d", w.Code)
			}
			var public config.Document
			decodeJSON(t, w, &public)
			if public.Data["api"].(map[string]any)["token"] != apiTokenMask {
				t.Fatal("token not masked")
			}
			public.Data["log"] = map[string]any{"level": "debug"}
			body, err := json.Marshal(map[string]any{"baseHash": public.Hash, "data": public.Data})
			if err != nil {
				t.Fatal(err)
			}
			w = send(http.MethodPut, "/api/v1/config", string(body))
			if w.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
			}
			saved, err := os.ReadFile(doc.Path)
			if err != nil || !bytes.Contains(saved, []byte("${TEST_TOKEN}")) || bytes.Contains(saved, []byte(secret)) || bytes.Contains(saved, []byte(apiTokenMask)) {
				t.Fatal("saved config lost the reference or persisted a secret/mask")
			}
			backup, err := os.ReadFile(doc.Path + ".bak")
			if err != nil || !bytes.Equal(backup, original) {
				t.Fatal("backup not preserved")
			}
			cfg, err := config.Load(doc.Path)
			if err != nil || cfg.API.Token != secret || cfg.Log.Level != "debug" {
				t.Fatal("round-trip changed token semantics")
			}
		})
	}
}

func TestManagerConfigPathCollisionReturns422(t *testing.T) {
	m, lockPath, original := configRoundTripManager(t, "api: {}")
	handler := NewHandler(Options{Config: m})
	for _, target := range []string{m.Meta().ConfigPath, m.Meta().ConfigPath + ".bak", lockPath} {
		doc, err := m.ConfigDocument()
		if err != nil {
			t.Fatal(err)
		}
		doc.Data["state"].(map[string]any)["path"] = target
		body, err := json.Marshal(map[string]any{"data": doc.Data})
		if err != nil {
			t.Fatal(err)
		}
		w := configRequest(handler, http.MethodPost, "/api/v1/config/validate", string(body), "application/json")
		var validation struct {
			Valid bool `json:"valid"`
		}
		decodeJSON(t, w, &validation)
		if w.Code != http.StatusOK || validation.Valid {
			t.Fatal("collision validated")
		}
		body, err = json.Marshal(map[string]any{"data": doc.Data, "baseHash": doc.Hash})
		if err != nil {
			t.Fatal(err)
		}
		w = configRequest(handler, http.MethodPut, "/api/v1/config", string(body), "application/json")
		assertJSONError(t, w, http.StatusUnprocessableEntity)
		got, err := os.ReadFile(doc.Path)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatal("rejected edit changed config")
		}
		got, err = os.ReadFile(lockPath)
		if err != nil || string(got) != "lock sentinel" {
			t.Fatal("rejected edit changed lock")
		}
		if _, err := os.Stat(doc.Path + ".bak"); !os.IsNotExist(err) {
			t.Fatal("rejected edit touched backup")
		}
	}
}
