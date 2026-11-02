package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"ros-ddns/internal/app"
	"ros-ddns/internal/config"
	"ros-ddns/internal/logging"
	"ros-ddns/internal/notify"
	"ros-ddns/internal/service"
)

var (
	_ func(Options) http.Handler                                           = NewHandler
	_ func(context.Context, config.APIConfig, Options) error               = Run
	_ func(context.Context, config.APIConfig) (net.Listener, error)        = Listen
	_ func(context.Context, config.APIConfig, Options, net.Listener) error = Serve
	_ Source                                                               = (*app.Manager)(nil)
	_ ConfigEditor                                                         = (*app.Manager)(nil)
	_ MetaSource                                                           = (*app.Manager)(nil)
	_ LogStore                                                             = (*logging.Hub)(nil)
	_ NotificationStore                                                    = (*notify.Store)(nil)
)

type fakeMeta struct {
	meta  app.Meta
	calls int
}

func (f *fakeMeta) Meta() app.Meta {
	f.calls++
	return f.meta
}

func TestMeta(t *testing.T) {
	started := time.Now().Add(-time.Minute).UTC()
	want := app.Meta{Version: "w4a", ConfigPath: "managed.yaml", StartedAt: started,
		Uptime: time.Minute, LogLevel: "debug"}
	for _, token := range []string{"", "secret"} {
		t.Run("token="+token, func(t *testing.T) {
			meta := &fakeMeta{meta: want}
			source := &fakeSource{snapshot: service.Snapshot{ConfigPath: "fallback.yaml"}}
			w := request(NewHandler(Options{Source: source, Meta: meta, Token: token, Version: "ignored"}),
				http.MethodGet, "/api/v1/meta", "Bearer "+token)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var result struct {
				app.Meta
				AuthRequired bool `json:"authRequired"`
			}
			decodeJSON(t, w, &result)
			if !reflect.DeepEqual(result.Meta, want) || result.AuthRequired != (token != "") || meta.calls != 1 || source.snapshotCalls != 0 {
				t.Fatalf("meta result = %+v, source calls = %d", result, source.snapshotCalls)
			}
		})
	}
	source := &fakeSource{snapshot: service.Snapshot{ConfigPath: "fallback.yaml", StartedAt: started}}
	w := request(NewHandler(Options{Source: source, Version: "fallback-version"}), http.MethodGet, "/api/v1/meta", "")
	var result struct {
		app.Meta
		AuthRequired bool `json:"authRequired"`
	}
	decodeJSON(t, w, &result)
	if result.Version != "fallback-version" || result.ConfigPath != "fallback.yaml" || !result.StartedAt.Equal(started) ||
		result.Uptime < time.Minute || result.Uptime > time.Since(started) || result.LogLevel != "info" || result.AuthRequired {
		t.Fatalf("fallback meta = %+v", result)
	}
	for _, opts := range []Options{{}, {Meta: &fakeMeta{}}} {
		w = request(NewHandler(opts), http.MethodGet, "/api/v1/meta", "")
		var fields map[string]json.RawMessage
		decodeJSON(t, w, &fields)
		for _, name := range []string{"version", "configPath", "startedAt", "uptime", "logLevel", "authRequired"} {
			if _, ok := fields[name]; !ok {
				t.Errorf("missing meta field %q", name)
			}
		}
		if _, ok := fields["dryRun"]; ok {
			t.Errorf("meta must not expose the removed dryRun field")
		}
	}
}

func TestStaticUI(t *testing.T) {
	files := fstest.MapFS{
		"index.html":        {Data: []byte(`<!doctype html><script src="/assets/main.js"></script>`)},
		"main.js":           {Data: []byte(`console.log("ready");`)},
		"main.css":          {Data: []byte(`body { color: black; }`)},
		"icons/home.svg":    {Data: []byte(`<svg></svg>`)},
		"folder/index.html": {Data: []byte("directory index must not be served")},
	}
	handler := NewHandler(Options{Token: "private-api", UIFS: files})
	for _, tc := range []struct{ path, name, cache, contentType string }{
		{"/", "index.html", "no-store", "text/html"},
		{"/assets/main.js", "main.js", "no-cache", "javascript"},
		{"/assets/main.css", "main.css", "no-cache", "text/css"},
		{"/assets/icons/home.svg", "icons/home.svg", "no-cache", "image/svg+xml"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := request(handler, http.MethodGet, tc.path, "")
			if w.Code != http.StatusOK || w.Body.String() != string(files[tc.name].Data) {
				t.Fatalf("response = %d %q", w.Code, w.Body.String())
			}
			for name, value := range map[string]string{
				"Cache-Control":           tc.cache,
				"Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; frame-ancestors 'none'; base-uri 'none'",
				"X-Content-Type-Options":  "nosniff", "Referrer-Policy": "no-referrer",
			} {
				if got := w.Header().Get(name); got != value {
					t.Errorf("%s = %q, want %q", name, got, value)
				}
			}
			if !strings.Contains(w.Header().Get("Content-Type"), tc.contentType) {
				t.Errorf("Content-Type = %q", w.Header().Get("Content-Type"))
			}
		})
	}
	for _, path := range []string{"/missing", "/index.html", "/assets/missing.js", "/assets/", "/assets/icons", "/assets/icons/", "/assets/folder/", "/assets/%2e%2e/index.html", "/assets/icons%2f..%2fmain.js", "/assets/icons%5c..%5cmain.js"} {
		t.Run("missing"+path, func(t *testing.T) {
			assertJSONError(t, request(handler, http.MethodGet, path, ""), http.StatusNotFound)
		})
	}
	for _, path := range []string{"/", "/assets/main.js"} {
		assertJSONError(t, request(NewHandler(Options{}), http.MethodGet, path, ""), http.StatusNotFound)
	}
	w := request(handler, http.MethodHead, "/", "")
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD index = %d %q", w.Code, w.Body.String())
	}
}

func TestAllAPIRoutesRequireAuthentication(t *testing.T) {
	handler := NewHandler(Options{Token: "secret"})
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/status"}, {http.MethodGet, "/api/v1/meta"},
		{http.MethodGet, "/api/v1/config"}, {http.MethodPost, "/api/v1/config/validate"},
		{http.MethodPut, "/api/v1/config"}, {http.MethodPost, "/api/v1/items/home/sync"},
		{http.MethodGet, "/api/v1/logs"}, {http.MethodGet, "/api/v1/logs/stream"},
		{http.MethodGet, "/api/v1/notifications"}, {http.MethodGet, "/api/v1/notifications/stream"},
		{http.MethodDelete, "/api/v1/config"}, {http.MethodGet, "/api/v1/unknown"},
	} {
		assertUnauthorized(t, request(handler, route.method, route.path, ""))
	}
}

func TestUnavailableDependencies(t *testing.T) {
	handler := NewHandler(Options{})
	for _, path := range []string{"/api/v1/status", "/api/v1/config", "/api/v1/logs", "/api/v1/logs/stream", "/api/v1/notifications", "/api/v1/notifications/stream"} {
		assertJSONError(t, request(handler, http.MethodGet, path, ""), http.StatusServiceUnavailable)
	}
	assertJSONError(t, request(handler, http.MethodPost, "/api/v1/items/home/sync", ""), http.StatusServiceUnavailable)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/config/validate", `{"data":{}}`},
		{http.MethodPut, "/api/v1/config", `{"baseHash":"hash","data":{}}`},
	} {
		assertJSONError(t, configRequest(handler, route.method, route.path, route.body, "application/json"), http.StatusServiceUnavailable)
	}
}
