package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExpandEnv expands ${NAME} and ${NAME:-default} references in a string.
// Parse applies it to YAML scalar values, never comments or raw YAML syntax.
// Only braced references are expanded so passwords containing a bare $
// keep working. "$$" is an escape for a literal "$".
//
// A reference without a default fails when the variable is unset; a variable
// that is set (even to the empty string) always wins over its default.
func ExpandEnv(input string) (string, error) {
	if !strings.Contains(input, "$") {
		return input, nil
	}

	var b strings.Builder
	b.Grow(len(input))

	for i := 0; i < len(input); {
		if input[i] != '$' {
			b.WriteByte(input[i])
			i++
			continue
		}

		// "$$" -> literal "$"
		if i+1 < len(input) && input[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}

		// Only ${...} is treated as a reference; a bare "$" is copied as-is.
		if i+1 >= len(input) || input[i+1] != '{' {
			b.WriteByte('$')
			i++
			continue
		}

		end := strings.IndexByte(input[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated environment reference at byte %d", i)
		}
		expr := input[i+2 : i+2+end]
		i = i + 2 + end + 1

		name := expr
		defaultValue := ""
		hasDefault := false
		if idx := strings.Index(expr, ":-"); idx >= 0 {
			name = expr[:idx]
			defaultValue = expr[idx+2:]
			hasDefault = true
		}
		if !validEnvName(name) {
			return "", fmt.Errorf("invalid environment variable name %q", name)
		}

		if value, ok := os.LookupEnv(name); ok {
			b.WriteString(value)
		} else if hasDefault {
			b.WriteString(defaultValue)
		} else {
			return "", fmt.Errorf("environment variable %s is not set (use ${%s:-default} for an optional value)", name, name)
		}
	}

	return b.String(), nil
}

func expandEnvScalars(root *yaml.Node) (map[*yaml.Node]bool, map[string]struct{}, error) {
	visited := make(map[*yaml.Node]bool)
	changed := make(map[*yaml.Node]bool)
	values := make(map[string]struct{})
	var visit func(*yaml.Node) error
	visit = func(node *yaml.Node) error {
		if node == nil || visited[node] {
			return nil
		}
		visited[node] = true
		if node.Kind == yaml.ScalarNode {
			value, err := ExpandEnv(node.Value)
			if err != nil {
				return err
			}
			// A reference can expand to its own spelling; it is still an
			// expanded string and must be included in error redaction.
			if value != node.Value || strings.Contains(node.Value, "${") {
				node.Value = value
				node.Tag = "!!str"
				// Quoting preserves whitespace, newlines and YAML punctuation.
				node.Style = yaml.DoubleQuotedStyle
				changed[node] = true
				values[value] = struct{}{}
			}
		}
		for _, child := range node.Content {
			if err := visit(child); err != nil {
				return err
			}
		}
		// Aliases share their anchor node; visit it once, including cycles.
		return visit(node.Alias)
	}
	err := visit(root)
	return changed, values, err
}

// scalarReference retains the original node even when reached through an alias.
type scalarReference struct{ node *yaml.Node }

func (r *scalarReference) UnmarshalYAML(node *yaml.Node) error {
	r.node = node
	return nil
}

// normalizeExpandedBooleans accepts environment-expanded scalars for the
// direct boolean fields of this schema when they expand to true or false, and
// retags those nodes so YAML decoding produces a bool. Values expanded through
// aliases or merge keys are intentionally left alone: those paths keep the
// strict string behavior and are rejected by the decoder.
func normalizeExpandedBooleans(document *yaml.Node, expanded map[*yaml.Node]bool) error {
	if len(expanded) == 0 || document == nil || len(document.Content) == 0 {
		return nil
	}
	root := document.Content[0]
	normalize := func(name string, node *yaml.Node) error {
		if node == nil || node.Kind != yaml.ScalarNode || !expanded[node] {
			return nil
		}
		value := strings.ToLower(strings.TrimSpace(node.Value))
		if value != "true" && value != "false" {
			return fmt.Errorf("%s: environment-expanded value must be true or false", name)
		}
		node.Value = value
		node.Tag = "!!bool"
		node.Style = 0
		return nil
	}
	if runtime := mappingValue(root, "runtime"); runtime != nil {
		if err := normalize("runtime.syncOnStart", mappingValue(runtime, "syncOnStart")); err != nil {
			return err
		}
	}
	if routerOS := mappingValue(root, "routeros"); routerOS != nil {
		if tls := mappingValue(routerOS, "tls"); tls != nil {
			if err := normalize("routeros.tls.insecureSkipVerify", mappingValue(tls, "insecureSkipVerify")); err != nil {
				return err
			}
		}
	}
	if items := mappingValue(root, "items"); items != nil && items.Kind == yaml.SequenceNode {
		for i, item := range items.Content {
			if err := normalize(fmt.Sprintf("items[%d].disabled", i), mappingValue(item, "disabled")); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateExpandedBooleanValues rejects effective boolean fields whose
// environment-expanded value is not true or false. It runs after decoding
// because aliases and merge keys resolve to their effective scalar only then.
func validateExpandedBooleanValues(document *yaml.Node, expanded map[*yaml.Node]bool) error {
	if len(expanded) == 0 || document == nil {
		return nil
	}
	var fields struct {
		Runtime struct {
			SyncOnStart scalarReference `yaml:"syncOnStart"`
		} `yaml:"runtime"`
		RouterOS struct {
			TLS struct {
				InsecureSkipVerify scalarReference `yaml:"insecureSkipVerify"`
			} `yaml:"tls"`
		} `yaml:"routeros"`
		Items []struct {
			Disabled scalarReference `yaml:"disabled"`
		} `yaml:"items"`
	}
	if err := document.Decode(&fields); err != nil {
		return fmt.Errorf("parse YAML: cannot validate environment-expanded boolean fields")
	}
	check := func(name string, ref scalarReference) error {
		if ref.node == nil || !expanded[ref.node] {
			return nil
		}
		value := strings.ToLower(strings.TrimSpace(ref.node.Value))
		if value != "true" && value != "false" {
			return fmt.Errorf("%s: environment-expanded value must be true or false", name)
		}
		return nil
	}
	if err := check("runtime.syncOnStart", fields.Runtime.SyncOnStart); err != nil {
		return err
	}
	if err := check("routeros.tls.insecureSkipVerify", fields.RouterOS.TLS.InsecureSkipVerify); err != nil {
		return err
	}
	for i, item := range fields.Items {
		if err := check(fmt.Sprintf("items[%d].disabled", i), item.Disabled); err != nil {
			return err
		}
	}
	return nil
}

// mappingValue returns the value node for key in a direct mapping, following
// no aliases or merge keys.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
