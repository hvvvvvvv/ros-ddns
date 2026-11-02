package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestCheckCLIIntegration verifies that -check validates a complete
// configuration without contacting RouterOS or a DNS provider.
func TestCheckCLIIntegration(t *testing.T) {
	var requests atomic.Int32
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(router.Close)

	path := writeCLIIntegrationConfig(t, router.URL, "3s")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-config", path, "-check"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, summary := range []string{"items:", "  - v4:", "  - v6:", "home.example.com A", "home.example.net A", "home6.example.com AAAA"} {
		if !strings.Contains(stdout.String(), summary) {
			t.Errorf("-check summary missing %q: %s", summary, stdout.String())
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("RouterOS requests during -check = %d, want 0", got)
	}
}

func writeCLIIntegrationConfig(t *testing.T, routerURL, timeout string) string {
	t.Helper()
	dir := t.TempDir()
	contents := fmt.Sprintf(`routeros:
  url: %q
  username: test-user
  password: long-test-password
  timeout: %s
dns:
  providers:
    ali:
      type: aliyun
      endpoint: alidns.aliyuncs.com
      accessKeyId: test-key-id
      accessKeySecret: test-key-secret
state:
  path: %q
items:
  - name: v4
    source: {interface: wan, family: ipv4}
    domains:
      - {provider: ali, domain: example.com, rr: home}
      - {provider: ali, domain: example.net, rr: home}
    addressList: {list: ddns-v4}
  - name: v6
    source:
      interface: wan
      family: ipv6
      eui64-mac: "00:1A:2B:3C:4D:5E"
      select: {scope: any}
    domains:
      - {provider: ali, domain: example.com, rr: home6}
    addressList: {list: ddns-v6}
`, routerURL, timeout, filepath.ToSlash(filepath.Join(dir, "state.json")))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
