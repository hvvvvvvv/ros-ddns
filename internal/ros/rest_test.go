package ros

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ros-ddns/internal/config"
)

func testRESTClient(t *testing.T, rawURL string, configure ...func(*config.RouterOSConfig)) Client {
	t.Helper()
	cfg := config.RouterOSConfig{
		URL:      rawURL,
		Username: "ddns-api",
		Password: "p:a ss&word",
		Timeout:  config.Duration{Duration: 3 * time.Second},
	}
	for _, change := range configure {
		change(&cfg)
	}
	client, err := NewRESTClient(cfg)
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}
	t.Cleanup(client.(*restClient).httpClient.CloseIdleConnections)
	return client
}

func checkRESTRequest(t *testing.T, r *http.Request, method, path string, query url.Values) {
	t.Helper()
	if r.Method != method || r.URL.Path != path {
		t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, method, path)
	}
	if r.URL.RawQuery != query.Encode() {
		t.Errorf("query = %q, want %q", r.URL.RawQuery, query.Encode())
	}
	username, password, ok := r.BasicAuth()
	if !ok || username != "ddns-api" || password != "p:a ss&word" {
		t.Error("missing or incorrect HTTP Basic Auth")
	}
	if got := r.Header.Get("User-Agent"); got != "ros-ddns/1" {
		t.Errorf("User-Agent = %q, want ros-ddns/1", got)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
	if method == http.MethodPut && r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
	}
}

func TestRESTClientAddresses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		family     Family
		basePath   string
		path       string
		properties string
		body       string
		want       []Address
	}{
		{
			name: "IPv4 with normalized base path", family: IPv4,
			basePath: "/gateway//router///", path: "/gateway/router/rest/ip/address",
			properties: ".id,address,interface,actual-interface,disabled,invalid",
			body: `[
				{".id":"*1","address":"203.0.113.23/24","interface":"pppoe-out1","actual-interface":"ether1","dynamic":"yes","disabled":"false","invalid":"no"},
				{".id":"*2","address":"192.168.1.7/24","interface":"bridge","dynamic":"no","disabled":"true","invalid":"yes"}
			]`,
			want: []Address{
				{Prefix: netip.MustParsePrefix("203.0.113.23/24"), Interface: "pppoe-out1", ActualInterface: "ether1"},
				{Prefix: netip.MustParsePrefix("192.168.1.7/24"), Interface: "bridge", Disabled: true, Invalid: true},
			},
		},
		{
			name: "IPv6 flags", family: IPv6, basePath: "/", path: "/rest/ipv6/address",
			properties: ".id,address,interface,actual-interface,disabled,invalid,global,link-local,deprecated",
			body: `[
				{".id":"*A","address":"2001:db8:1::42/64","interface":"pppoe-out1","actual-interface":"ether1","dynamic":"true","disabled":"no","invalid":"","global":"yes","link-local":"false","deprecated":"no"},
				{".id":"*B","address":"fe80::1234/64","interface":"ether1","dynamic":"false","global":"no","link-local":"yes","deprecated":"true"},
				{".id":"*C","address":"fd00::7/64","interface":"bridge","global":" TRUE ","deprecated":"YES"}
			]`,
			want: []Address{
				{Prefix: netip.MustParsePrefix("2001:db8:1::42/64"), Interface: "pppoe-out1", ActualInterface: "ether1", Global: true},
				{Prefix: netip.MustParsePrefix("fe80::1234/64"), Interface: "ether1", LinkLocal: true, Deprecated: true},
				{Prefix: netip.MustParsePrefix("fd00::7/64"), Interface: "bridge", Global: true, Deprecated: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkRESTRequest(t, r, http.MethodGet, tc.path, url.Values{".proplist": {tc.properties}})
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			got, err := testRESTClient(t, server.URL+tc.basePath).Addresses(context.Background(), tc.family)
			if err != nil {
				t.Fatalf("Addresses: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Addresses = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRESTClientListEntries(t *testing.T) {
	for _, tc := range []struct {
		family Family
		root   string
	}{
		{IPv4, "ip"}, {IPv6, "ipv6"},
	} {
		t.Run(string(tc.family), func(t *testing.T) {
			const list = "home v4&v6/+?=中文"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkRESTRequest(t, r, http.MethodGet, "/rest/"+tc.root+"/firewall/address-list", url.Values{
					"list": {list}, ".proplist": {".id,list,address,comment,timeout,dynamic,disabled"},
				})
				_ = json.NewEncoder(w).Encode([]map[string]string{
					{".id": "*1", "list": list, "address": "203.0.113.12", "comment": "ros-ddns:home", "timeout": "1h", "dynamic": "yes", "disabled": "no"},
					{".id": "*2", "list": list, "address": "2001:db8::42/64", "dynamic": "false", "disabled": "true"},
				})
			}))
			t.Cleanup(server.Close)
			got, err := testRESTClient(t, server.URL).ListEntries(context.Background(), tc.family, list)
			if err != nil {
				t.Fatalf("ListEntries: %v", err)
			}
			want := []ListEntry{
				{ID: "*1", List: list, Address: "203.0.113.12", Comment: "ros-ddns:home", Timeout: "1h", Dynamic: true},
				{ID: "*2", List: list, Address: "2001:db8::42/64", Disabled: true},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ListEntries = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRESTClientAddEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		family  Family
		root    string
		address string
		timeout string
		status  int
	}{
		{"static IPv4", IPv4, "ip", "203.0.113.42", "", http.StatusCreated},
		{"expiring IPv4", IPv4, "ip", "203.0.113.42", "1h", http.StatusOK},
		{"static IPv6", IPv6, "ipv6", "2001:db8::42", "", http.StatusNoContent},
		{"expiring IPv6", IPv6, "ipv6", "2001:db8::42", "30m", http.StatusAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkRESTRequest(t, r, http.MethodPut, "/rest/"+tc.root+"/firewall/address-list", nil)
				var got map[string]string
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode PUT body: %v", err)
				}
				want := map[string]string{"list": "home & office", "address": tc.address, "comment": ""}
				if tc.timeout != "" {
					want["timeout"] = tc.timeout
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("PUT body = %v, want %v", got, want)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			err := testRESTClient(t, server.URL).AddEntry(context.Background(), tc.family, ListEntry{
				ID: "*ignored", List: "home & office", Address: tc.address, Timeout: tc.timeout, Dynamic: true, Disabled: true,
			})
			if err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
		})
	}
}

func TestRESTClientAddEntryDuplicateSentinel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
		detail  string
		exists  bool
	}{
		{"duplicate", 400, "failure: already have such entry", "", true},
		{"duplicate case insensitive", 400, "Already have such entry", "", true},
		{"other bad request", 400, "invalid address", "", false},
		{"other status", 409, "already have such entry", "", false},
		{"server error mentioning duplicate", 500, "already have such entry", "", false},
		{"phrase only in detail", 400, "invalid request", "already have such entry", true},
		{"server detail mentioning duplicate", 500, "backend failed", "already have such entry", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": tc.status, "message": tc.message, "detail": tc.detail})
			}))
			t.Cleanup(server.Close)
			err := testRESTClient(t, server.URL).AddEntry(context.Background(), IPv4, ListEntry{List: "home", Address: "203.0.113.42"})
			if err == nil || errors.Is(err, ErrEntryExists) != tc.exists {
				t.Fatalf("AddEntry error = %v, want duplicate sentinel %v", err, tc.exists)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || apiErr.Message != tc.message || apiErr.Detail != tc.detail {
				t.Errorf("AddEntry error = %v, want unchanged APIError for status %d", err, tc.status)
			}
		})
	}
}

func TestRESTClientRemoveEntry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		family    Family
		root      string
		id        string
		escapedID string
		status    int
		wantErr   bool
	}{
		{"IPv4", IPv4, "ip", "*1", "*1", 200, false},
		{"IPv6", IPv6, "ipv6", "*1", "*1", 204, false},
		{"already absent", IPv4, "ip", "*1", "*1", 404, false},
		{"escaped segment", IPv6, "ipv6", "*1/a b?#%", "*1%2Fa%20b%3F%23%25", 200, false},
		{"forbidden", IPv4, "ip", "*1", "*1", 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "/gateway/rest/" + tc.root + "/firewall/address-list/"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkRESTRequest(t, r, http.MethodDelete, prefix+tc.id, nil)
				if got := r.URL.EscapedPath(); got != prefix+tc.escapedID {
					t.Errorf("DELETE escaped path = %q, want %q", got, prefix+tc.escapedID)
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)
			err := testRESTClient(t, server.URL+"/gateway/").RemoveEntry(context.Background(), tc.family, tc.id)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemoveEntry error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestRESTClientRejectsWriteRedirects(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		noLocation      bool
		invalidLocation bool
	}{
		{"301", http.StatusMovedPermanently, false, false},
		{"302", http.StatusFound, false, false},
		{"303", http.StatusSeeOther, false, false},
		{"307", http.StatusTemporaryRedirect, false, false},
		{"308", http.StatusPermanentRedirect, false, false},
		{"302 without Location", http.StatusFound, true, false},
		{"302 malformed Location", http.StatusFound, false, true},
		{"304", http.StatusNotModified, true, false},
	} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				var requests, redirected atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						redirected.Add(1)
						w.WriteHeader(http.StatusOK) // Would falsely report a successful write.
						return
					}
					requests.Add(1)
					path := "/rest/ip/firewall/address-list"
					if method == http.MethodDelete {
						path += "/*1"
					}
					checkRESTRequest(t, r, method, path, nil)
					if !tc.noLocation {
						path := "/redirected"
						if tc.invalidLocation {
							path = "/%zz"
						}
						w.Header().Set("Location", "http://redirect-user:redirect-password@"+r.Host+path+"?token=query-secret#fragment-secret")
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, "body-secret")
				}))
				t.Cleanup(server.Close)
				client := testRESTClient(t, server.URL)
				var err error
				if method == http.MethodPut {
					err = client.AddEntry(context.Background(), IPv4, ListEntry{List: "home", Address: "203.0.113.42"})
				} else {
					err = client.RemoveEntry(context.Background(), IPv4, "*1")
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || !strings.Contains(err.Error(), "redirect refused") {
					t.Fatalf("error = %v, want visible HTTP %d redirect failure", err, tc.status)
				}
				if requests.Load() != 1 || redirected.Load() != 0 {
					t.Fatalf("requests = %d, redirected = %d; want 1, 0", requests.Load(), redirected.Load())
				}
				if !tc.noLocation && !tc.invalidLocation && !strings.Contains(err.Error(), server.URL+"/redirected") {
					t.Fatalf("error lacks sanitized Location: %v", err)
				}
				for _, secret := range []string{"ddns-api", "p:a ss&word", "redirect-user", "redirect-password", "query-secret", "fragment-secret", "body-secret"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("redirect error leaked %q: %v", secret, err)
					}
				}
			})
		}
	}
}

func TestRESTClientRejectsHTTPSDowngrade(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)
	client := testRESTClient(t, source.URL, func(cfg *config.RouterOSConfig) { cfg.TLS.InsecureSkipVerify = true })
	err := client.AddEntry(context.Background(), IPv4, ListEntry{List: "home", Address: "203.0.113.42"})
	if err == nil || redirected.Load() != 0 {
		t.Fatalf("downgrade error = %v, target requests = %d", err, redirected.Load())
	}
}

func TestRESTClientAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   APIError
	}{
		{"unauthorized", 401, `{"error":401,"message":"Unauthorized","detail":"bad credentials"}`, APIError{StatusCode: 401, Message: "Unauthorized", Detail: "bad credentials"}},
		{"forbidden", 403, `{"error":403,"message":"Forbidden","detail":"policy denied"}`, APIError{StatusCode: 403, Message: "Forbidden", Detail: "policy denied"}},
		{"string code", 400, `{"error":"INVALID","message":"Bad Request","detail":"invalid list"}`, APIError{StatusCode: 400, Message: "Bad Request", Detail: "invalid list"}},
		{"HTTP and body codes differ", 502, `{"error":500,"message":"Router failed"}`, APIError{StatusCode: 502, Message: "Router failed"}},
		{"plain text", 502, " upstream failed\n", APIError{StatusCode: 502, Message: "Bad Gateway", Detail: "upstream failed"}},
		{"empty body", 404, "", APIError{StatusCode: 404, Message: "Not Found"}},
		{"detail only", 500, `{"detail":"router error"}`, APIError{StatusCode: 500, Message: "Internal Server Error", Detail: "router error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			_, err := testRESTClient(t, server.URL).Addresses(context.Background(), IPv4)
			var got *APIError
			if !errors.As(err, &got) {
				t.Fatalf("error = %v, want APIError", err)
			}
			if *got != tc.want {
				t.Errorf("APIError = %#v, want %#v", *got, tc.want)
			}
			for _, part := range []string{fmt.Sprint(tc.status), tc.want.Message, tc.want.Detail} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
			if tc.status == 401 || tc.status == 403 {
				for _, policy := range []string{"rest-api", "read", "write"} {
					if !strings.Contains(err.Error(), policy) {
						t.Errorf("error %q does not mention %s policy", err, policy)
					}
				}
			}
		})
	}
}

func TestRESTClientTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkRESTRequest(t, r, http.MethodGet, "/rest/ip/address", url.Values{
			".proplist": {".id,address,interface,actual-interface,disabled,invalid"},
		})
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(server.Close)

	t.Run("insecure", func(t *testing.T) {
		client := testRESTClient(t, server.URL, func(cfg *config.RouterOSConfig) {
			cfg.TLS.InsecureSkipVerify = true
		})
		if _, err := client.Addresses(context.Background(), IPv4); err != nil {
			t.Fatalf("Addresses over insecure TLS: %v", err)
		}
	})
	t.Run("custom CA", func(t *testing.T) {
		caFile := filepath.Join(t.TempDir(), "router-ca.pem")
		data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		if err := os.WriteFile(caFile, data, 0600); err != nil {
			t.Fatal(err)
		}
		client := testRESTClient(t, server.URL, func(cfg *config.RouterOSConfig) {
			cfg.TLS.CAFile = caFile
		})
		if _, err := client.Addresses(context.Background(), IPv4); err != nil {
			t.Fatalf("Addresses with trusted custom CA: %v", err)
		}
	})
}

func TestNewRESTClientInvalidConfig(t *testing.T) {
	for _, rawURL := range []string{"", "/router", "ftp://localhost", "https://", "http://["} {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := NewRESTClient(config.RouterOSConfig{URL: rawURL}); err == nil {
				t.Fatal("expected invalid URL error")
			}
		})
	}
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"missing", "", "read RouterOS TLS CA file"},
		{"invalid", "not a PEM certificate", "no valid PEM certificates"},
		{"invalid DER", "-----BEGIN CERTIFICATE-----\nYWJj\n-----END CERTIFICATE-----\n", "no valid PEM certificates"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caFile := filepath.Join(t.TempDir(), "ca.pem")
			if tc.data != "" {
				if err := os.WriteFile(caFile, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := NewRESTClient(config.RouterOSConfig{URL: "https://127.0.0.1", TLS: config.TLSConfig{CAFile: caFile}})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), fmt.Sprintf("%q", caFile)) {
				t.Errorf("error = %v, want %q and CA file path", err, tc.want)
			}
		})
	}
}

func TestRESTClientMalformedResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"invalid JSON", `[{`, "decode RouterOS GET ip/address response"},
		{"invalid prefix", `[{".id":"*9","address":"not-a-prefix"}]`, `address "not-a-prefix" (id "*9")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			_, err := testRESTClient(t, server.URL).Addresses(context.Background(), IPv4)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRESTClientRejectsInvalidFamily(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid family should not send a request")
	}))
	t.Cleanup(server.Close)
	client := testRESTClient(t, server.URL)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Addresses":   func() error { _, err := client.Addresses(ctx, "invalid"); return err },
		"ListEntries": func() error { _, err := client.ListEntries(ctx, "invalid", "home"); return err },
		"AddEntry":    func() error { return client.AddEntry(ctx, "invalid", ListEntry{}) },
		"RemoveEntry": func() error { return client.RemoveEntry(ctx, "invalid", "*1") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || !strings.Contains(err.Error(), "unsupported RouterOS address family") {
				t.Errorf("error = %v, want invalid family error", err)
			}
		})
	}
}

func TestRESTClientContextAndTimeout(t *testing.T) {
	for _, mode := range []string{"caller cancellation", "client timeout"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })
			client := testRESTClient(t, server.URL, func(cfg *config.RouterOSConfig) {
				if mode == "client timeout" {
					cfg.Timeout.Duration = 100 * time.Millisecond
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.Addresses(ctx, IPv4); done <- err }()
			if mode == "caller cancellation" {
				select {
				case <-started:
					cancel()
				case <-time.After(5 * time.Second):
					t.Fatal("request did not reach test server")
				}
			}
			select {
			case err := <-done:
				want := context.Canceled
				if mode == "client timeout" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) || !strings.Contains(err.Error(), "RouterOS GET ip/address") {
					t.Errorf("error = %v, want contextual error wrapping %v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request did not stop")
			}
		})
	}
}
