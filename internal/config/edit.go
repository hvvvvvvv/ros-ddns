package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Document is an editable, unexpanded configuration and the hash of its exact
// file bytes. Data does not retain YAML comments, anchors or presentation style.
// It is not a redacted view: literal credentials remain literal credentials.
type Document struct {
	Path string         `json:"path"`
	Data map[string]any `json:"data"`
	Hash string         `json:"hash"`
	// APITokenValue is the effective parsed token for restoring an unquoted
	// scalar token (numeric/bool/timestamp) that cannot be represented
	// faithfully in Data. It is never serialized by the config HTTP API.
	APITokenValue string `json:"-"`
	// APITokenString records whether api.token was originally a YAML string
	// scalar. Only then should the unexpanded Data value (env reference or
	// literal) be restored verbatim; other scalar tags use APITokenValue.
	APITokenString bool `json:"-"`
}

// ReadDocument reads exactly one YAML mapping without expanding environment
// references or requiring those variables to exist. Use ValidateStructured to
// check the configuration schema, defaults and semantics before saving edits.
func ReadDocument(path string) (Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("read config document: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return Document{}, fmt.Errorf("decode config document: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Document{}, fmt.Errorf("config must contain exactly one YAML document")
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Document{}, fmt.Errorf("config document root must be an object")
	}
	if err := checkDocumentKeys(&node, make(map[*yaml.Node]bool)); err != nil {
		return Document{}, err
	}
	var data map[string]any
	if err := node.Decode(&data); err != nil {
		return Document{}, fmt.Errorf("decode config object: %w", err)
	}
	data, err = normalizeStructured(data)
	if err != nil {
		return Document{}, err
	}
	hash := sha256.Sum256(raw)
	_, tokenIsString := yamlStringScalarAt(&node, "api", "token")
	return Document{
		Path: path, Data: data, Hash: hex.EncodeToString(hash[:]),
		APITokenString: tokenIsString,
	}, nil
}

// yamlStringScalarAt reports whether the value at a mapping path is a YAML
// string scalar. Explicit keys override merges; earlier merge sequence entries
// override later ones. Reuse the decoder's merge/alias and duplicate-key rules
// while scalarReference retains each effective value's original YAML tag.
func yamlStringScalarAt(root *yaml.Node, keys ...string) (*yaml.Node, bool) {
	node := root
	for _, key := range keys {
		var fields map[string]scalarReference
		if node == nil || node.Decode(&fields) != nil {
			return nil, false
		}
		node = fields[key].node
	}
	if node == nil || node.Kind != yaml.ScalarNode {
		return node, false
	}
	return node, node.Tag == "!!str"
}

// DecodeStructured decodes exactly one JSON object. Numbers are normalized
// recursively to int64 when exactly integral and representable, or to finite
// float64 otherwise. Large integers are read without an intermediate float64.
func DecodeStructured(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode structured config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("structured config must contain exactly one JSON object")
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("structured config root must be an object")
	}
	normalized, err := normalizeNumbers(object)
	if err != nil {
		return nil, err
	}
	return normalized.(map[string]any), nil
}

// MarshalStructured writes deterministic YAML from a non-nil JSON-compatible
// object. It normalizes a copy, so callers retain ownership of their input.
func MarshalStructured(data map[string]any) ([]byte, error) {
	normalized, err := normalizeStructured(data)
	if err != nil {
		return nil, err
	}
	encoded, err := yaml.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal structured config: %w", err)
	}
	return encoded, nil
}

// ValidateStructured is the authoritative validation path for JSON edits. The
// returned YAML retains environment references; only Config contains expanded
// values. Parse applies strict field checks, defaults and all semantic checks.
func ValidateStructured(data []byte, path string) (*Config, []byte, error) {
	object, err := DecodeStructured(data)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := MarshalStructured(object)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := Parse(encoded, path)
	if err != nil {
		return nil, nil, err
	}
	return cfg, encoded, nil
}

func normalizeStructured(data map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("config must contain JSON-compatible values: %w", err)
	}
	return DecodeStructured(encoded)
}

func normalizeNumbers(value any) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			normalized, err := normalizeNumbers(child)
			if err != nil {
				return nil, err
			}
			v[key] = normalized
		}
	case []any:
		for i, child := range v {
			normalized, err := normalizeNumbers(child)
			if err != nil {
				return nil, err
			}
			v[i] = normalized
		}
	case json.Number:
		if integer, err := v.Int64(); err == nil {
			return integer, nil
		}
		floating, err := v.Float64()
		if err != nil || math.IsNaN(floating) || math.IsInf(floating, 0) {
			return nil, fmt.Errorf("structured config number is outside the supported range")
		}
		if floating == 0 {
			// Underflow must not turn a nonzero fraction into an integral zero.
			mantissa, _, _ := strings.Cut(strings.ToLower(string(v)), "e")
			if strings.Trim(mantissa, "-+.0") == "" {
				return int64(0), nil
			}
		} else if math.Trunc(floating) == floating && floating >= math.MinInt64 && floating <= math.MaxInt64 {
			// Check the exact decimal, including exponent notation, rather than
			// rounding e.g. 9007199254740993.0 through float64 first.
			exact, ok := new(big.Rat).SetString(string(v))
			if ok && exact.IsInt() && exact.Num().IsInt64() {
				return exact.Num().Int64(), nil
			}
		}
		return floating, nil
	}
	return value, nil
}

func checkDocumentKeys(node *yaml.Node, visited map[*yaml.Node]bool) error {
	if node == nil || visited[node] {
		return nil
	}
	visited[node] = true
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || (key.Tag != "!!str" && key.Tag != "!!merge") {
				return fmt.Errorf("config object keys must be strings")
			}
		}
	}
	for _, child := range node.Content {
		if err := checkDocumentKeys(child, visited); err != nil {
			return err
		}
	}
	return checkDocumentKeys(node.Alias, visited)
}
