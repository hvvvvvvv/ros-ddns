package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestApplyStartupFixupsPreservesDocument(t *testing.T) {
	t.Setenv("WRITEBACK_LEVEL", "info")
	t.Setenv("WRITEBACK_PASSWORD", "private-value")
	t.Setenv("WRITEBACK_BACKOFF", "1s")
	const raw = `# file heading ${COMMENT_ONLY}
log:
  # level key comment
  level: ${WRITEBACK_LEVEL} # keep environment
  # format key comment
  format: xml # format inline comment
runtime:
  pollInterval: 0s # poll inline comment
  maxBackoff: ${WRITEBACK_BACKOFF}
routeros:
  url: unsupported-url # optional invalid setting
  password: ${WRITEBACK_PASSWORD}
api:
  listen: 0.0.0.0:8080 # remote API
items: [{disabled: true}] # incomplete optional item
# file footer
`
	startup := mustStartup(t, raw)
	out, err := ApplyStartupFixups([]byte(raw), startup.Fixups)
	if err != nil {
		t.Fatalf("ApplyStartupFixups: %v", err)
	}
	for _, preserved := range []string{
		"# file heading ${COMMENT_ONLY}", "# level key comment", "# keep environment",
		"# format key comment", "# format inline comment", "# poll inline comment",
		"# optional invalid setting", "# remote API", "# incomplete optional item", "# file footer",
		"${WRITEBACK_LEVEL}", "${WRITEBACK_PASSWORD}", "${WRITEBACK_BACKOFF}",
	} {
		if !bytes.Contains(out, []byte(preserved)) {
			t.Errorf("lost original comment or placeholder %q", preserved)
		}
	}
	if bytes.Contains(out, []byte("private-value")) || !bytes.HasSuffix(out, []byte("\n")) {
		t.Fatal("output expanded a secret or omitted final newline")
	}
	var before, after map[string]any
	if err := yaml.Unmarshal([]byte(raw), &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	before["log"].(map[string]any)["format"] = "text"
	before["runtime"].(map[string]any)["pollInterval"] = "10s"
	before["api"].(map[string]any)["token"] = startup.Config.API.Token
	if !reflect.DeepEqual(before, after) {
		t.Fatal("writeback changed fields other than writable fixups")
	}
	document, err := parseYAMLDocument(out)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 0; i < len(document.Content[0].Content); i += 2 {
		keys = append(keys, document.Content[0].Content[i].Value)
	}
	if !reflect.DeepEqual(keys, []string{"log", "runtime", "routeros", "api", "items"}) {
		t.Fatalf("root order changed: %v", keys)
	}
	again, err := ParseStartup(out, startup.Config.Path)
	if err != nil {
		t.Fatalf("ParseStartup after writeback: %v", err)
	}
	if again.Config.API.Token != startup.Config.API.Token || len(again.Fixups) != 1 || again.Fixups[0].Path != "runtime.maxBackoff" || again.Fixups[0].Write {
		t.Fatal("only the preserved invalid environment reference should still need fallback")
	}
}

func TestApplyStartupFixupsNoWriteIsExact(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("# unchanged\r\nlog: {level: info}"), []byte("not valid [YAML")} {
		for _, fixups := range [][]Fixup{nil, {{Path: "log.level", Value: "info", Write: false}}} {
			out, err := ApplyStartupFixups(raw, fixups)
			if err != nil || !bytes.Equal(out, raw) || (raw != nil && &out[0] != &raw[0]) {
				t.Fatal("no writable fixups must return the original bytes directly")
			}
		}
	}
	if (*Startup)(nil).NeedsWrite() || (&Startup{}).NeedsWrite() {
		t.Fatal("empty or nil startup must not need writeback")
	}
}

func TestApplyStartupFixupsTokenInsertionAndReplacement(t *testing.T) {
	t.Setenv("WRITEBACK_TOKEN", "")
	for _, raw := range []string{
		"api: {listen: '0.0.0.0:8080'}\n",
		"api: {listen: '0.0.0.0:8080', token: null}\n",
		"api: {listen: '0.0.0.0:8080', token: '${WRITEBACK_TOKEN}'}\n",
		"routeros: {password: &password 'bad token'}\napi: {listen: '0.0.0.0:8080', token: *password}\n",
		"<<: {api: &settings {listen: '0.0.0.0:8080'}}\napi: *settings\n",
		"<<: {api: {listen: '0.0.0.0:8080'}}\n",
	} {
		t.Run(raw, func(t *testing.T) {
			startup := mustStartup(t, raw)
			out, err := ApplyStartupFixups([]byte(raw), startup.Fixups)
			if err != nil {
				t.Fatalf("ApplyStartupFixups: %v", err)
			}
			again, err := ParseStartup(out, startup.Config.Path)
			if err != nil {
				t.Fatalf("ParseStartup after writeback: %v", err)
			}
			if len(again.Fixups) != 0 || again.Config.API.Token != startup.Config.API.Token || again.Config.API.Listen != startup.Config.API.Listen || again.Config.RouterOS.Password != startup.Config.RouterOS.Password {
				t.Fatal("token was not persisted or unrelated effective values changed")
			}
		})
	}
}

func TestApplyStartupFixupsLiteralRoundTrip(t *testing.T) {
	const raw = `log: {level: '', format: invalid}
runtime: {pollInterval: 0s, stableFor: -1s, reconcileTimeout: 0s, verifyInterval: -1s, maxBackoff: 0s}
state: {path: "bad\x00path"}
api: {listen: '', token: 'bad token'}
items: [{}]
`
	startup := mustStartup(t, raw)
	if len(startup.Fixups) != 10 {
		t.Fatalf("got %d fixups, want 10", len(startup.Fixups))
	}
	out, err := ApplyStartupFixups([]byte(raw), startup.Fixups)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseStartup(out, startup.Config.Path)
	if err != nil || len(again.Fixups) != 0 {
		t.Fatalf("round trip must not generate new fixups: err=%v", err)
	}
	for _, fixup := range startup.Fixups {
		if got := startupValue(again.Config, fixup.Path); got != fixup.Value {
			t.Errorf("%s after round trip = %q, want %q", fixup.Path, got, fixup.Value)
		}
	}
}

func TestApplyStartupFixupsMaxBackoffRoundTrip(t *testing.T) {
	t.Setenv("WRITEBACK_POLL", "10h")
	for _, tc := range []struct{ name, raw string }{
		{"missing", "runtime:\n  pollInterval: 10h\n"},
		{"null", "runtime:\n  pollInterval: 10h\n  maxBackoff: null\n"},
		{"merged section", "<<: {runtime: {pollInterval: 10h}}\n"},
		{"aliased section", "<<: {runtime: &intervals {pollInterval: 10h}}\nruntime: *intervals\n"},
		{"merged null", "runtime:\n  <<: {pollInterval: 10h, maxBackoff: null}\n"},
		{"environment poll", "runtime:\n  pollInterval: ${WRITEBACK_POLL}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startup := mustStartup(t, tc.raw)
			out, err := ApplyStartupFixups([]byte(tc.raw), startup.Fixups)
			if err != nil {
				t.Fatalf("ApplyStartupFixups: %v", err)
			}
			if !bytes.Contains(out, []byte("maxBackoff: 10h0m0s")) {
				t.Fatalf("missing persisted maxBackoff: %s", out)
			}
			if strings.Contains(tc.raw, "${WRITEBACK_POLL}") && !bytes.Contains(out, []byte("${WRITEBACK_POLL}")) {
				t.Fatal("writeback expanded the pollInterval environment reference")
			}
			again, err := ParseStartup(out, startup.Config.Path)
			if err != nil {
				t.Fatalf("ParseStartup after writeback: %v", err)
			}
			if len(again.Fixups) != 0 || again.Config.Runtime.MaxBackoff.Duration.String() != "10h0m0s" || again.Config.Runtime.PollInterval.Duration.String() != "10h0m0s" {
				t.Fatalf("backoff did not survive writeback: runtime=%+v, fixups=%+v", again.Config.Runtime, again.Fixups)
			}
			if _, err := Parse(out, startup.Config.Path); err != nil {
				t.Fatalf("strict Parse after writeback: %v", err)
			}
		})
	}
}

func TestApplyStartupFixupsPreservesAnchorUsers(t *testing.T) {
	const raw = "log: {level: &level invalid}\nrouteros: {password: *level}\n"
	startup := mustStartup(t, raw)
	out, err := ApplyStartupFixups([]byte(raw), startup.Fixups)
	if err != nil {
		t.Fatal(err)
	}
	again := mustStartup(t, string(out))
	if len(again.Fixups) != 0 || again.Config.Log.Level != "info" || again.Config.RouterOS.Password != "invalid" {
		t.Fatal("rewriting an anchored scalar changed an unrelated alias")
	}
}

func TestApplyStartupFixupsPreservesEnvironmentKeys(t *testing.T) {
	t.Setenv("WRITEBACK_API_KEY", "api")
	t.Setenv("WRITEBACK_TOKEN_KEY", "token")
	t.Setenv("WRITEBACK_FORMAT_KEY", "format")
	t.Setenv("WRITEBACK_TOKEN_VALUE", "")
	const raw = "${WRITEBACK_API_KEY}:\n  listen: 0.0.0.0:8080\n  ${WRITEBACK_TOKEN_KEY}: ${WRITEBACK_TOKEN_VALUE}\nlog:\n  ${WRITEBACK_FORMAT_KEY}: invalid\n"
	startup := mustStartup(t, raw)
	if len(startup.Fixups) != 2 || !startup.Fixups[0].Write || !startup.Fixups[1].Write {
		t.Fatal("literal format and generated token must be writable through environment keys")
	}
	out, err := ApplyStartupFixups([]byte(raw), startup.Fixups)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"${WRITEBACK_API_KEY}", "${WRITEBACK_TOKEN_KEY}", "${WRITEBACK_FORMAT_KEY}"} {
		if !bytes.Contains(out, []byte(key)) {
			t.Errorf("lost key placeholder %s", key)
		}
	}
	again := mustStartup(t, string(out))
	if len(again.Fixups) != 0 || again.Config.API.Token != startup.Config.API.Token || again.Config.Log.Format != "text" {
		t.Fatal("environment keys did not round trip")
	}
}

func TestApplyStartupFixupsTypesAndErrors(t *testing.T) {
	out, err := ApplyStartupFixups([]byte("api: null\nruntime: {}\n"), []Fixup{
		{Path: "api.token", Value: "123456", Write: true, Secret: true},
		{Path: "runtime.syncOnStart", Value: "false", Write: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := yaml.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["api"].(map[string]any)["token"] != "123456" || fields["runtime"].(map[string]any)["syncOnStart"] != false {
		t.Fatal("fixups lost YAML string/bool types")
	}
	for _, raw := range []string{"", "[]", "{}\n---\n{}", "log: [", "log: {level: bad, level: other}", "log: {level: []}", "log: []"} {
		if _, err := ApplyStartupFixups([]byte(raw), []Fixup{{Path: "log.level", Value: "info", Write: true}}); err == nil {
			t.Errorf("writeback accepted malformed input %q", raw)
		}
	}
	for _, fixup := range []Fixup{
		{Path: "routeros.password", Value: "secret", Write: true},
		{Path: "runtime.syncOnStart", Value: "not-bool-secret", Write: true},
	} {
		if _, err := ApplyStartupFixups([]byte("{}"), []Fixup{fixup}); err == nil || strings.Contains(err.Error(), fixup.Value) {
			t.Fatalf("invalid fixup must fail without including its value: %v", err)
		}
	}
}
