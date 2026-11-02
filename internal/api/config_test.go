package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ros-ddns/internal/app"
	"ros-ddns/internal/config"
	"ros-ddns/internal/service"
)

type fakeConfigEditor struct {
	document                  config.Document
	documentErr               error
	validateErr, applyErr     error
	snapshot                  service.Snapshot
	warnings                  []string
	validated, applied        []byte
	baseHash                  string
	applyContext              context.Context
	documentCalls             int
	validateCalls, applyCalls int
}

func (f *fakeConfigEditor) ConfigDocument() (config.Document, error) {
	f.documentCalls++
	return f.document, f.documentErr
}

func (f *fakeConfigEditor) ValidateConfig(data []byte) error {
	f.validateCalls++
	f.validated = append([]byte(nil), data...)
	return f.validateErr
}

func (f *fakeConfigEditor) ApplyConfig(ctx context.Context, hash string, data []byte) (service.Snapshot, []string, error) {
	f.applyCalls++
	f.applyContext, f.baseHash = ctx, hash
	f.applied = append([]byte(nil), data...)
	return f.snapshot, f.warnings, f.applyErr
}

func configRequest(handler http.Handler, method, path, body, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func assertJSONError(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var body map[string]string
	decodeJSON(t, w, &body)
	if body["error"] == "" {
		t.Error("missing JSON error")
	}
}

func TestConfigDocument(t *testing.T) {
	document := config.Document{Path: "config.yaml", Hash: "hash-1", Data: map[string]any{
		"api":      map[string]any{"token": "literal-secret"},
		"routeros": map[string]any{"password": "${ROUTER_PASSWORD}"},
	}}
	want := config.Document{Path: "config.yaml", Hash: "hash-1", Data: map[string]any{
		"api":      map[string]any{"token": apiTokenMask},
		"routeros": map[string]any{"password": "${ROUTER_PASSWORD}"},
	}}
	editor := &fakeConfigEditor{document: document}
	handler := NewHandler(Options{Config: editor})
	w := request(handler, http.MethodGet, "/api/v1/config", "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q", w.Code, w.Header().Get("Cache-Control"))
	}
	var got config.Document
	decodeJSON(t, w, &got)
	if !reflect.DeepEqual(got, want) || editor.documentCalls != 1 {
		t.Fatalf("document = %+v, calls = %d", got, editor.documentCalls)
	}
	if strings.Contains(w.Body.String(), "literal-secret") {
		t.Fatal("GET exposed the API token")
	}
	editor.documentErr = errors.New("document secret")
	w = request(handler, http.MethodGet, "/api/v1/config", "")
	assertJSONError(t, w, http.StatusInternalServerError)
	if strings.Contains(w.Body.String(), "document secret") {
		t.Fatal("document error leaked backend details")
	}
}

func TestConfigDocumentMasksNumericAPIToken(t *testing.T) {
	editor := &fakeConfigEditor{document: config.Document{Path: "config.yaml", Hash: "hash-1", Data: map[string]any{
		"api": map[string]any{"token": 987654321},
	}}}
	w := request(NewHandler(Options{Config: editor}), http.MethodGet, "/api/v1/config", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "987654321") {
		t.Fatalf("numeric token leaked: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Data map[string]any `json:"data"`
	}
	decodeJSON(t, w, &got)
	api, _ := got.Data["api"].(map[string]any)
	if api["token"] != apiTokenMask {
		t.Fatalf("numeric token was not masked: %v", api["token"])
	}
}

func TestConfigValidate(t *testing.T) {
	const data = `{"routeros":{"password":"${ROUTER_PASSWORD}"},"integer":9007199254740993}`
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			editor := &fakeConfigEditor{}
			if invalid {
				editor.validateErr = errors.New("literal credential must stay private")
			}
			w := configRequest(NewHandler(Options{Config: editor}), http.MethodPost,
				"/api/v1/config/validate", `{"data":`+data+`}`, "application/json; charset=utf-8")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var result struct {
				Valid  bool     `json:"valid"`
				Errors []string `json:"errors"`
			}
			decodeJSON(t, w, &result)
			if result.Valid == invalid || (len(result.Errors) > 0) != invalid {
				t.Errorf("validation result = %+v", result)
			}
			if string(editor.validated) != data || editor.validateCalls != 1 || editor.applyCalls != 0 {
				t.Fatalf("validator did not receive the original JSON object: %s", editor.validated)
			}
			if strings.Contains(w.Body.String(), "literal credential") {
				t.Error("validation error leaked a credential")
			}
		})
	}
}

func TestConfigApply(t *testing.T) {
	const data = `{"api":{"token":"new-secret"},"number":9007199254740993}`
	want := service.Snapshot{ConfigPath: "saved.yaml", Items: []service.ItemStatus{}}
	editor := &fakeConfigEditor{snapshot: want, warnings: []string{"formatting normalized"}}
	r := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"baseHash":"hash-1","data":`+data+`}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://"+r.Host)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	NewHandler(Options{Config: editor}).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Snapshot service.Snapshot `json:"snapshot"`
		Warnings []string         `json:"warnings"`
	}
	decodeJSON(t, w, &result)
	if !reflect.DeepEqual(result.Snapshot, want) || !reflect.DeepEqual(result.Warnings, editor.warnings) {
		t.Errorf("apply result = %+v", result)
	}
	if editor.baseHash != "hash-1" || string(editor.applied) != data || editor.applyCalls != 1 || editor.applyContext != ctx {
		t.Fatalf("wrong apply arguments: %+v", editor)
	}
	editor.warnings = nil
	w = configRequest(NewHandler(Options{Config: editor}), http.MethodPut, "/api/v1/config", `{"baseHash":"hash-1","data":{}}`, "application/json")
	decodeJSON(t, w, &result)
	if result.Warnings == nil {
		t.Error("warnings must be an array, not null")
	}
}

func TestConfigApplyRestoresMaskedAPIToken(t *testing.T) {
	editor := &fakeConfigEditor{document: config.Document{
		Path: "config.yaml", Hash: "hash-1", APITokenString: true, APITokenValue: "expanded-runtime-secret",
		Data: map[string]any{"api": map[string]any{"token": "${ROS_DDNS_API_TOKEN}"}},
	}}
	body := `{"baseHash":"hash-1","data":{"api":{"token":"` + apiTokenMask + `","listen":"127.0.0.1:8080"},"number":9007199254740993,"log":{"level":"info"}}}`
	w := configRequest(NewHandler(Options{Config: editor}), http.MethodPut, "/api/v1/config", body, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var applied struct {
		API    map[string]any `json:"api"`
		Number int64          `json:"number"`
	}
	if err := json.Unmarshal(editor.applied, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.API["token"] != "${ROS_DDNS_API_TOKEN}" {
		t.Fatalf("token was not restored before apply: %v", applied.API["token"])
	}
	if applied.Number != 9007199254740993 {
		t.Fatalf("large integer lost precision: %d", applied.Number)
	}
	if strings.Contains(string(editor.applied), apiTokenMask) {
		t.Fatal("mask reached ApplyConfig")
	}
}

func TestConfigApplyRestoresUnquotedScalarToken(t *testing.T) {
	editor := &fakeConfigEditor{document: config.Document{
		Path: "config.yaml", Hash: "hash-1", APITokenValue: "TRUE",
		Data: map[string]any{"api": map[string]any{"token": true}},
	}}
	body := `{"baseHash":"hash-1","data":{"api":{"token":"` + apiTokenMask + `"}}}`
	w := configRequest(NewHandler(Options{Config: editor}), http.MethodPut, "/api/v1/config", body, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var applied struct {
		API map[string]any `json:"api"`
	}
	if err := json.Unmarshal(editor.applied, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.API["token"] != "TRUE" {
		t.Fatalf("effective scalar token was not restored: %v", applied.API["token"])
	}
}

func TestConfigApplyRestoresTimestampScalarToken(t *testing.T) {
	editor := &fakeConfigEditor{document: config.Document{
		Path: "config.yaml", Hash: "hash-1", APITokenValue: "2000-01-01",
		Data: map[string]any{"api": map[string]any{"token": "2000-01-01T00:00:00Z"}},
	}}
	body := `{"baseHash":"hash-1","data":{"api":{"token":"` + apiTokenMask + `"}}}`
	w := configRequest(NewHandler(Options{Config: editor}), http.MethodPut, "/api/v1/config", body, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var applied struct {
		API map[string]any `json:"api"`
	}
	if err := json.Unmarshal(editor.applied, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.API["token"] != "2000-01-01" {
		t.Fatalf("effective timestamp token was not restored: %v", applied.API["token"])
	}
}

func TestConfigValidateRestoresMaskedAPIToken(t *testing.T) {
	editor := &fakeConfigEditor{document: config.Document{Path: "config.yaml", Hash: "hash-1", APITokenValue: "expanded-runtime-secret", APITokenString: true, Data: map[string]any{
		"api": map[string]any{"token": "${ROS_DDNS_API_TOKEN}"},
	}}}
	body := `{"data":{"api":{"token":"` + apiTokenMask + `"}}}`
	w := configRequest(NewHandler(Options{Config: editor}), http.MethodPost, "/api/v1/config/validate", body, "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var validated struct {
		API map[string]any `json:"api"`
	}
	if err := json.Unmarshal(editor.validated, &validated); err != nil {
		t.Fatal(err)
	}
	if validated.API["token"] != "${ROS_DDNS_API_TOKEN}" {
		t.Fatalf("validator received the mask: %v", validated.API["token"])
	}
}

func TestConfigApplyErrors(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		applyErr, validateErr error
		status                int
	}{
		{"conflict", fmt.Errorf("secret: %w", app.ErrConfigConflict), nil, http.StatusConflict},
		{"restart", fmt.Errorf("%w: secret", app.ErrRestartRequired), nil, http.StatusUnprocessableEntity},
		{"validation", errors.New("secret invalid value"), errors.New("secret invalid value"), http.StatusUnprocessableEntity},
		{"io", errors.New("secret file error"), nil, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			editor := &fakeConfigEditor{applyErr: tc.applyErr, validateErr: tc.validateErr}
			w := configRequest(NewHandler(Options{Config: editor}), http.MethodPut, "/api/v1/config", `{"baseHash":"hash","data":{}}`, "application/json")
			assertJSONError(t, w, tc.status)
			if strings.Contains(w.Body.String(), "secret") || editor.applyCalls != 1 {
				t.Errorf("unsafe error or missing apply call: %s", w.Body.String())
			}
		})
	}
}

func TestConfigRejectsMalformedRequests(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/config/validate"},
		{http.MethodPut, "/api/v1/config"},
	} {
		for _, tc := range []struct {
			name, body, contentType string
			status                  int
		}{
			{"missing content type", `{"data":{}}`, "", 415},
			{"text", `{"data":{}}`, "text/plain", 415},
			{"invalid content type", `{"data":{}}`, "application/json; broken", 415},
			{"empty", "", "application/json", 400},
			{"syntax", `{"data":{`, "application/json", 400},
			{"trailing json", `{"data":{}} {}`, "application/json", 400},
			{"trailing junk", `{"data":{}} secret`, "application/json", 400},
			{"null root", `null`, "application/json", 400},
			{"array root", `[]`, "application/json", 400},
			{"missing data", `{}`, "application/json", 400},
			{"null data", `{"data":null}`, "application/json", 400},
			{"array data", `{"data":[]}`, "application/json", 400},
			{"scalar data", `{"data":"secret"}`, "application/json", 400},
			{"unknown field", `{"data":{},"secret":true}`, "application/json", 400},
			{"oversize", `{"data":{"padding":"` + strings.Repeat("x", maxConfigBody) + `"}}`, "application/json", 413},
			{"oversize whitespace", `{"data":{}}` + strings.Repeat(" ", maxConfigBody), "application/json", 413},
		} {
			t.Run(route.method+"/"+tc.name, func(t *testing.T) {
				editor := &fakeConfigEditor{}
				w := configRequest(NewHandler(Options{Config: editor}), route.method, route.path, tc.body, tc.contentType)
				assertJSONError(t, w, tc.status)
				if editor.validateCalls != 0 || editor.applyCalls != 0 || strings.Contains(w.Body.String(), "secret") {
					t.Error("invalid request reached editor or leaked submitted data")
				}
			})
		}
	}
	for _, body := range []string{`{"data":{}}`, `{"baseHash":null,"data":{}}`, `{"baseHash":42,"data":{}}`} {
		w := configRequest(NewHandler(Options{Config: &fakeConfigEditor{}}), http.MethodPut, "/api/v1/config", body, "application/json")
		assertJSONError(t, w, http.StatusBadRequest)
	}
}

func TestConfigBodyLimitInclusive(t *testing.T) {
	for _, route := range []struct{ method, path, prefix string }{
		{http.MethodPost, "/api/v1/config/validate", `{"data":{"padding":"`},
		{http.MethodPut, "/api/v1/config", `{"baseHash":"hash","data":{"padding":"`},
	} {
		const suffix = `"}}`
		body := route.prefix + strings.Repeat("x", maxConfigBody-len(route.prefix)-len(suffix)) + suffix
		w := configRequest(NewHandler(Options{Config: &fakeConfigEditor{}}), route.method, route.path, body, "application/json")
		if w.Code != http.StatusOK {
			t.Fatalf("exactly 1 MiB %s: status = %d: %s", route.method, w.Code, w.Body.String())
		}
	}
}

func TestSameOriginMutations(t *testing.T) {
	for _, route := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/api/v1/items/home/sync", "", http.StatusAccepted},
		{http.MethodPut, "/api/v1/config", `{"baseHash":"hash","data":{}}`, http.StatusOK},
	} {
		for _, tc := range []struct {
			name      string
			origins   []string
			forwarded string
			allowed   bool
		}{
			{"absent", nil, "", true},
			{"matching", []string{"http://example.com:8080"}, "", true},
			{"case", []string{"http://EXAMPLE.COM:8080"}, "", true},
			{"scheme", []string{"https://example.com:8080"}, "", false},
			{"forwarded scheme", []string{"https://example.com:8080"}, "https", true},
			{"foreign", []string{"http://attacker.test"}, "", false},
			{"port", []string{"http://example.com"}, "", false},
			{"null", []string{"null"}, "", false},
			{"empty", []string{""}, "", false},
			{"relative", []string{"//example.com:8080"}, "", false},
			{"invalid", []string{"http://%"}, "", false},
			{"userinfo", []string{"http://secret@example.com:8080"}, "", false},
			{"path", []string{"http://example.com:8080/path"}, "", false},
			{"query", []string{"http://example.com:8080?secret"}, "", false},
			{"duplicate", []string{"http://example.com:8080", "http://example.com:8080"}, "", false},
		} {
			t.Run(route.method+"/"+tc.name, func(t *testing.T) {
				source := &fakeSource{accepted: map[string]bool{"home": true}}
				editor := &fakeConfigEditor{}
				r := httptest.NewRequest(route.method, "http://example.com:8080"+route.path, strings.NewReader(route.body))
				r.Header.Set("Content-Type", "application/json")
				if tc.forwarded != "" {
					r.Header.Set("X-Forwarded-Proto", tc.forwarded)
				}
				for _, origin := range tc.origins {
					r.Header.Add("Origin", origin)
				}
				w := httptest.NewRecorder()
				NewHandler(Options{Source: source, Config: editor}).ServeHTTP(w, r)
				if tc.allowed {
					if w.Code != route.status || len(source.triggered)+editor.applyCalls != 1 {
						t.Fatalf("same-origin request failed: %d %s", w.Code, w.Body.String())
					}
				} else {
					assertJSONError(t, w, http.StatusForbidden)
					if len(source.triggered)+editor.applyCalls != 0 {
						t.Error("cross-origin mutation reached backend")
					}
				}
			})
		}
	}
}

func TestConfigErrorsDoNotLogSecrets(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	defer slog.SetDefault(previous)
	editor := &fakeConfigEditor{applyErr: errors.New("credential-private"), validateErr: errors.New("credential-private")}
	handler := NewHandler(Options{Config: editor})
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/config/validate", `{"data":{"password":"credential-private"}}`},
		{http.MethodPut, "/api/v1/config", `{"baseHash":"hash","data":{"password":"credential-private"}}`},
	} {
		w := configRequest(handler, route.method, route.path, route.body, "application/json")
		if strings.Contains(w.Body.String(), "credential-private") {
			t.Fatal("response contains credentials")
		}
	}
	if strings.Contains(output.String(), "credential-private") {
		t.Fatal("logs contain request credentials")
	}
}
