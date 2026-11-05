package config

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ApplyStartupFixups updates only writable startup fallbacks in the original,
// unexpanded YAML. It retains comments, mapping order and environment references
// in other fields. Encoding normalizes indentation and adds a final newline.
// With no writable fixups, raw is returned unchanged without even decoding it.
func ApplyStartupFixups(raw []byte, fixups []Fixup) ([]byte, error) {
	if !(&Startup{Fixups: fixups}).NeedsWrite() {
		return raw, nil
	}
	document, err := parseYAMLDocument(raw)
	if err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config document root must be an object")
	}
	// Check duplicates and aliases without interpreting duration strings,
	// expanding placeholders, or validating optional settings.
	var fields map[string]any
	if err := document.Decode(&fields); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	root := document.Content[0]
	for _, fixup := range fixups {
		if !fixup.Write {
			continue
		}
		tag, ok := startupFixupTag(fixup.Path)
		if !ok {
			return nil, fmt.Errorf("unsupported startup fixup path %q", fixup.Path)
		}
		if tag == "!!bool" && fixup.Value != "true" && fixup.Value != "false" {
			return nil, fmt.Errorf("%s: startup fixup must be a boolean", fixup.Path)
		}
		section, key, _ := strings.Cut(fixup.Path, ".")
		mapping, err := writebackMapping(root, section)
		if err != nil {
			return nil, err
		}
		node := writebackValue(mapping, key)
		if node == nil {
			node = &yaml.Node{Kind: yaml.ScalarNode}
			mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
		}
		if node.Kind != yaml.ScalarNode && node.Kind != yaml.AliasNode {
			return nil, fmt.Errorf("%s: startup fixup target must be a scalar", fixup.Path)
		}
		// Changing an anchor must not change unrelated fields that reference it.
		// Materialize those scalar aliases with the old value before replacement.
		preserveScalarAliases(document, node)
		style := yaml.Style(0)
		if tag == "!!str" && (fixup.Value == "" || node.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) != 0) {
			style = yaml.DoubleQuotedStyle
		}
		node.Kind, node.Tag, node.Value, node.Style = yaml.ScalarNode, tag, fixup.Value, style
		node.Alias, node.Content = nil, nil
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	if _, err := parseYAMLDocument(out.Bytes()); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func startupFixupTag(path string) (string, bool) {
	switch path {
	case "runtime.syncOnStart":
		return "!!bool", true
	case "log.level", "log.format", "runtime.pollInterval", "runtime.stableFor",
		"runtime.reconcileTimeout", "runtime.verifyInterval", "runtime.maxBackoff",
		"state.path", "api.listen", "api.token":
		return "!!str", true
	default:
		return "", false
	}
}

// Resolve references in keys only to locate the same fields as ParseStartup.
// Neither the keys nor any unrelated scalar values are expanded in the output.
func writebackValue(mapping *yaml.Node, key string) *yaml.Node {
	if node := mappingValue(mapping, key); node != nil {
		return node
	}
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		name := mapping.Content[i]
		if name.Kind == yaml.ScalarNode && strings.Contains(name.Value, "${") {
			if expanded, err := ExpandEnv(name.Value); err == nil && expanded == key {
				return mapping.Content[i+1]
			}
		}
	}
	return nil
}

// writebackMapping creates a local mapping when a fixup needs an override of an
// aliased or merged section. Shared anchors stay unchanged.
func writebackMapping(root *yaml.Node, key string) (*yaml.Node, error) {
	node := writebackValue(root, key)
	if node != nil && node.Kind == yaml.MappingNode {
		return node, nil
	}
	var inherited *yaml.Node
	if node == nil {
		var fields map[string]scalarReference
		if err := root.Decode(&fields); err != nil {
			return nil, fmt.Errorf("parse YAML: %w", err)
		}
		inherited = fields[key].node
		node = &yaml.Node{}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, node)
	} else if node.Kind == yaml.AliasNode {
		inherited = node.Alias
	} else if node.Tag != "!!null" {
		return nil, fmt.Errorf("%s: startup fixup parent must be an object", key)
	}
	node.Kind, node.Tag, node.Value = yaml.MappingNode, "!!map", ""
	node.Style, node.Alias, node.Content = 0, nil, nil
	if inherited != nil && inherited.Tag != "!!null" {
		if inherited.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s: startup fixup parent must be an object", key)
		}
		var merge *yaml.Node
		if inherited.Anchor != "" {
			merge = &yaml.Node{Kind: yaml.AliasNode, Value: inherited.Anchor, Alias: inherited}
		} else {
			merge = copyYAMLWithoutAnchors(inherited)
		}
		node.Content = []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, merge}
	}
	return node, nil
}

func copyYAMLWithoutAnchors(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Anchor = ""
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = copyYAMLWithoutAnchors(child)
	}
	return &copy
}

func preserveScalarAliases(node, target *yaml.Node) {
	if target.Anchor == "" {
		return
	}
	if node.Kind == yaml.AliasNode && node.Alias == target {
		node.Kind, node.Tag, node.Value, node.Style = target.Kind, target.Tag, target.Value, target.Style
		node.Alias = nil
		return
	}
	for _, child := range node.Content {
		preserveScalarAliases(child, target)
	}
}
