package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func documentFromText(t *testing.T, text string) Document {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	document, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestReadDocumentPreservesReferencesAndRawHash(t *testing.T) {
	t.Setenv("EDIT_PASSWORD", "resolved-router-password")
	t.Setenv("EDIT_TOKEN", "resolved-api-token")
	const raw = "# ${NOT_NEEDED} comment\nrouteros:\n  password: &secret ${EDIT_PASSWORD}\napi:\n  token: ${EDIT_TOKEN}\ncopy: *secret\noptional: ${MISSING_EDIT_ENV:-fallback}\nescaped: $${LITERAL}\nnested: [3, 2.5]\n"
	document := documentFromText(t, raw)
	encoded, err := json.Marshal(document.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"${EDIT_PASSWORD}", "${EDIT_TOKEN}", "${MISSING_EDIT_ENV:-fallback}", "$${LITERAL}"} {
		if !strings.Contains(string(encoded), value) {
			t.Errorf("reference %q lost: %s", value, encoded)
		}
	}
	if strings.Contains(string(encoded), "resolved-") || document.Data["copy"] != "${EDIT_PASSWORD}" {
		t.Fatalf("document expanded a secret or lost its alias: %s", encoded)
	}
	values := document.Data["nested"].([]any)
	if values[0] != int64(3) || values[1] != float64(2.5) {
		t.Fatalf("unexpected number types: %#v", values)
	}
	hash := sha256.Sum256([]byte(raw))
	if document.Hash != hex.EncodeToString(hash[:]) || filepath.Base(document.Path) != "config.yaml" {
		t.Fatalf("incorrect file identity: %+v", document)
	}
	if err := os.WriteFile(document.Path, []byte(raw+"# changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := ReadDocument(document.Path)
	if err != nil || next.Hash == document.Hash || !reflect.DeepEqual(next.Data, document.Data) {
		t.Fatalf("hash must include comments: next=%+v err=%v", next, err)
	}
}

func TestReadDocumentTracksAPITokenScalarKind(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		wantString bool
	}{
		{"env reference", "api:\n  token: ${EDIT_TOKEN}\n", true},
		{"plain string", "api:\n  token: plain-token\n", true},
		{"quoted timestamp", "api:\n  token: \"2000-01-01\"\n", true},
		{"unquoted timestamp", "api:\n  token: 2000-01-01\n", false},
		{"numeric", "api:\n  token: 12345\n", false},
		{"boolean", "api:\n  token: TRUE\n", false},
		{"absent", "log:\n  level: info\n", false},
		{"inline merge", "api: {<<: {token: '${EDIT_TOKEN}'}}", true},
		{"alias merge", "defaults: &d {token: '${EDIT_TOKEN}'}\napi: {<<: *d}", true},
		{"scalar alias", "value: &t '${EDIT_TOKEN}'\napi: {<<: {token: *t}}", true},
		{"mapping alias", "defaults: &d {token: '${EDIT_TOKEN}'}\napi: *d", true},
		{"root merge", "<<: {api: {token: '${EDIT_TOKEN}'}}", true},
		{"nested merge", "api: {<<: {<<: {token: '${EDIT_TOKEN}'}}}", true},
		{"sequence first string", "api: {<<: [{token: '${EDIT_TOKEN}'}, {token: 123}]}", true},
		{"sequence first number", "api: {<<: [{token: 123}, {token: '${EDIT_TOKEN}'}]}", false},
		{"sequence alias", "defaults: &d {token: '${EDIT_TOKEN}'}\napi: {<<: [*d, {token: 123}]}", true},
		{"sequence skip missing", "api: {<<: [{listen: localhost}, {token: '${EDIT_TOKEN}'}]}", true},
		{"explicit string overrides merge", "api: {token: '${EDIT_TOKEN}', <<: {token: 123}}", true},
		{"explicit number overrides merge", "api: {<<: {token: '${EDIT_TOKEN}'}, token: 123}", false},
		{"explicit null overrides merge", "api: {<<: {token: '${EDIT_TOKEN}'}, token: null}", false},
		{"explicit mapping overrides merge", "<<: {api: {token: '${EDIT_TOKEN}'}}\napi: {listen: localhost}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := documentFromText(t, tc.raw)
			if document.APITokenString != tc.wantString {
				t.Fatalf("APITokenString = %v, want %v (data=%#v)", document.APITokenString, tc.wantString, document.Data)
			}
		})
	}
}

func TestReadDocumentRejectsInvalidDocuments(t *testing.T) {
	for _, raw := range []string{"", "# comment only", "null", "[]", "hello", "a: 1\n---\nb: 2", "{}\n---\n", "a: [", "a: 1\na: 2", "{1: value}", "nested: {false: value}", "a: &a [*a]"} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadDocument(path); err == nil {
				t.Fatal("invalid document accepted")
			}
		})
	}
	if _, err := ReadDocument(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing document accepted")
	}
}

func TestDecodeStructuredNormalizesNumbersRecursively(t *testing.T) {
	data, err := DecodeStructured([]byte(`{"integer":42,"fraction":1.25,"nested":[{"exponent":1e3,"decimal":9007199254740993.0,"near":1.0000000000000000000001}],"max":9223372036854775807,"min":-9223372036854775808,"wide":1e20,"zero":-0.0,"bool":true,"null":null}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"integer": int64(42), "fraction": float64(1.25),
		"nested": []any{map[string]any{"exponent": int64(1000), "decimal": int64(9007199254740993), "near": float64(1)}},
		"max":    int64(math.MaxInt64), "min": int64(math.MinInt64), "wide": float64(1e20),
		"zero": int64(0), "bool": true, "null": nil,
	}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("got %#v, want %#v", data, want)
	}
}

func TestDecodeStructuredRejectsInvalidInput(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", "true", "42", `"string"`, "{} {}", "{} garbage", "{}\n---\n{}", `{"x":1e9999}`, `{"x":NaN}`, "key: value"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := DecodeStructured([]byte(raw)); err == nil {
				t.Fatal("invalid JSON object accepted")
			}
		})
	}
}

func TestMarshalStructuredDeterministicAndNormalized(t *testing.T) {
	input := map[string]any{"z": json.Number("1e3"), "a": map[string]any{"x": 2, "password": "${EDIT_PASSWORD}"}}
	first, err := MarshalStructured(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalStructured(map[string]any{"a": input["a"], "z": int64(1000)})
	if err != nil || !bytes.Equal(first, second) || !bytes.Contains(first, []byte("z: 1000")) {
		t.Fatalf("non-deterministic or unnormalized YAML: %s / %s (%v)", first, second, err)
	}
	if input["z"] != json.Number("1e3") {
		t.Fatal("MarshalStructured mutated its input")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, invalid := range []map[string]any{nil, {"x": math.Inf(1)}, {"x": make(chan int)}, cycle} {
		if _, err := MarshalStructured(invalid); err == nil {
			t.Fatal("non-object or non-JSON value accepted")
		}
	}
}

func TestValidateStructuredUsesAuthoritativeParser(t *testing.T) {
	t.Setenv("EDIT_PASSWORD", "secret: # with YAML punctuation\nand a newline")
	document := documentFromText(t, "# ignored comment\n"+strings.Replace(providerConfig, "super-secret", "${EDIT_PASSWORD}", 1))
	data, _ := json.Marshal(document.Data)
	cfg, yamlBytes, err := ValidateStructured(data, document.Path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != document.Path || cfg.RouterOS.Password != os.Getenv("EDIT_PASSWORD") || cfg.Items[0].Domains[0].Domain != "example.com" || cfg.Items[0].Domains[0].TTL != 600 {
		t.Fatalf("parsing/defaults/normalization were bypassed: %+v", cfg)
	}
	if !bytes.Contains(yamlBytes, []byte("${EDIT_PASSWORD}")) || bytes.Contains(yamlBytes, []byte("secret:")) || bytes.Contains(yamlBytes, []byte("ignored comment")) {
		t.Fatalf("persisted YAML contains expanded data or comments: %s", yamlBytes)
	}
	parsed, err := Parse(yamlBytes, document.Path)
	if err != nil || !reflect.DeepEqual(parsed, cfg) {
		t.Fatalf("returned YAML does not validate identically: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"unknown root", func(d map[string]any) { d["typo"] = true }},
		{"unknown nested", func(d map[string]any) { d["routeros"].(map[string]any)["typo"] = true }},
		{"unknown provider", func(d map[string]any) {
			d["dns"].(map[string]any)["providers"].(map[string]any)["ali-main"].(map[string]any)["typo"] = 1
		}},
		{"invalid runtime", func(d map[string]any) { d["runtime"] = map[string]any{"pollInterval": "0s"} }},
		{"expanded integer", func(d map[string]any) {
			d["dns"].(map[string]any)["providers"].(map[string]any)["ali-main"].(map[string]any)["qps"] = "${EDIT_QPS:-5}"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object, err := DecodeStructured(data)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(object)
			bad, _ := json.Marshal(object)
			if cfg, yaml, err := ValidateStructured(bad, document.Path); err == nil || cfg != nil || yaml != nil {
				t.Fatalf("invalid edit produced a usable config: cfg=%v yaml=%q err=%v", cfg, yaml, err)
			}
		})
	}
}
