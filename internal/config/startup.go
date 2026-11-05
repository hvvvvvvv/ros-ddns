package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"gopkg.in/yaml.v3"

	"ros-ddns/internal/pathguard"
)

// Fixup describes a startup fallback. Reason never includes the submitted value.
type Fixup struct {
	Path   string // Dotted configuration path.
	Value  string // Replacement scalar text; durations use time.Duration.String.
	Reason string // Safe diagnostic without secret values.
	Secret bool   // Callers must not log Value in ordinary logs when true.
	Write  bool   // Whether the original YAML should be updated.
}

// Startup contains the effective configuration and its startup fallbacks.
type Startup struct {
	Config *Config
	Fixups []Fixup
}

// NeedsWrite reports whether at least one fallback needs to be persisted.
func (s *Startup) NeedsWrite() bool {
	if s != nil {
		for _, fixup := range s.Fixups {
			if fixup.Write {
				return true
			}
		}
	}
	return false
}

// ParseStartup strictly decodes one YAML document, then repairs invalid required
// settings. Optional settings are normalized but not semantically validated;
// callers can check them individually before starting their runtime components.
// This function does not write files. ApplyStartupFixups prepares the writeback.
func ParseStartup(data []byte, path string) (*Startup, error) {
	document, err := parseYAMLDocument(data)
	if err != nil {
		return nil, err
	}
	original := snapshotYAMLNodes(document)
	cfg, values, err := decodeConfig(document, path)
	if err != nil {
		return nil, err
	}
	cfg.Normalize()
	result := &Startup{Config: cfg}
	defaults := Default()
	fix := func(path, value, reason string, secret bool) {
		present, writable := startupScalar(document, path, original)
		if present {
			result.Fixups = append(result.Fixups, Fixup{
				Path: path, Value: value, Reason: reason, Secret: secret, Write: writable,
			})
		}
	}

	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		cfg.Log.Level = defaults.Log.Level
		fix("log.level", cfg.Log.Level, "must be debug, info, warn or error", false)
	}
	switch cfg.Log.Format {
	case "text", "json":
	default:
		cfg.Log.Format = defaults.Log.Format
		fix("log.format", cfg.Log.Format, "must be text or json", false)
	}
	for _, field := range []struct {
		path     string
		value    *Duration
		fallback Duration
		invalid  bool
		reason   string
	}{
		{"runtime.pollInterval", &cfg.Runtime.PollInterval, defaults.Runtime.PollInterval, cfg.Runtime.PollInterval.Duration <= 0, "must be greater than 0"},
		{"runtime.stableFor", &cfg.Runtime.StableFor, defaults.Runtime.StableFor, cfg.Runtime.StableFor.Duration < 0, "must not be negative"},
		{"runtime.reconcileTimeout", &cfg.Runtime.ReconcileTimeout, defaults.Runtime.ReconcileTimeout, cfg.Runtime.ReconcileTimeout.Duration <= 0, "must be greater than 0"},
		{"runtime.verifyInterval", &cfg.Runtime.VerifyInterval, defaults.Runtime.VerifyInterval, cfg.Runtime.VerifyInterval.Duration < 0, "must not be negative"},
	} {
		if field.invalid {
			field.value.Duration = field.fallback.Duration
			fix(field.path, field.value.Duration.String(), field.reason, false)
		}
	}
	if cfg.Runtime.MaxBackoff.Duration < cfg.Runtime.PollInterval.Duration {
		cfg.Runtime.MaxBackoff.Duration = max(defaults.Runtime.MaxBackoff.Duration, cfg.Runtime.PollInterval.Duration)
		present, writable := startupScalar(document, "runtime.maxBackoff", original)
		// Missing or null values need a persisted override when the default is
		// too short. Explicit references retain their normal writeback rules.
		result.Fixups = append(result.Fixups, Fixup{
			Path: "runtime.maxBackoff", Value: cfg.Runtime.MaxBackoff.Duration.String(),
			Reason: "must be greater than or equal to runtime.pollInterval", Write: !present || writable,
		})
	}
	if present, _ := startupScalar(document, "state.path", original); present && cfg.State.Path != "" {
		if err := pathguard.Validate(path, cfg.State.Path, ""); err != nil {
			cfg.State.Path = defaults.State.Path
			fix("state.path", cfg.State.Path, "must resolve without conflicting with the config or its backup", false)
		}
	}
	if cfg.API.Listen == "" {
		cfg.API.Listen = defaults.API.Listen
		fix("api.listen", cfg.API.Listen, "must not be empty", false)
	}
	host, port, err := net.SplitHostPort(cfg.API.Listen)
	if err != nil {
		return nil, redactEnvError(fmt.Errorf("invalid config:\n - api.listen: invalid address %q: %v", cfg.API.Listen, err), values)
	}
	if port == "" {
		return nil, fmt.Errorf("invalid config:\n - api.listen: port is required")
	}
	if !validListenPort(port) {
		return nil, fmt.Errorf("invalid config:\n - api.listen: port must be a decimal number between 1 and 65535")
	}
	if !loopbackListenHost(host) && (cfg.API.Token == "" || !validAPIToken(cfg.API.Token)) {
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, fmt.Errorf("api.token: cannot generate a startup token: %w", err)
		}
		cfg.API.Token = hex.EncodeToString(token[:])
		// A generated credential must survive restart, including when replacing
		// a placeholder, alias, null or missing token.
		result.Fixups = append(result.Fixups, Fixup{
			Path: "api.token", Value: cfg.API.Token, Secret: true, Write: true,
			Reason: "a valid token is required for a non-loopback listener",
		})
	} else if !validAPIToken(cfg.API.Token) {
		cfg.API.Token = ""
		fix("api.token", "", "must be at most 256 bytes of printable ASCII without spaces or control characters", true)
	}
	return result, nil
}

// Snapshot the graph before environment expansion mutates scalar tags and text.
// Keeping node identities also handles environment references in mapping keys.
func snapshotYAMLNodes(root *yaml.Node) map[*yaml.Node]yaml.Node {
	original := make(map[*yaml.Node]yaml.Node)
	var visit func(*yaml.Node)
	visit = func(node *yaml.Node) {
		if node == nil {
			return
		}
		if _, seen := original[node]; seen {
			return
		}
		original[node] = *node
		for _, child := range node.Content {
			visit(child)
		}
		visit(node.Alias)
	}
	visit(root)
	return original
}

// startupScalar finds the effective field but only allows writing a directly
// addressed literal scalar. Aliases and merged values remain read-only.
func startupScalar(document *yaml.Node, path string, original map[*yaml.Node]yaml.Node) (present, writable bool) {
	node := document
	writable = true
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	for _, key := range strings.Split(path, ".") {
		if node != nil && node.Kind == yaml.AliasNode {
			node = node.Alias
			writable = false
		}
		if node == nil || node.Kind != yaml.MappingNode {
			return false, false
		}
		direct := mappingValue(node, key)
		if direct == nil {
			var fields map[string]scalarReference
			if err := node.Decode(&fields); err != nil {
				return false, false // The strict decoder has already checked this.
			}
			node = fields[key].node
			writable = false
		} else {
			node = direct
		}
	}
	if node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
		writable = false
	}
	if node == nil || original[node].Tag == "!!null" {
		return false, false
	}
	raw := original[node]
	return true, writable && raw.Kind == yaml.ScalarNode && !strings.Contains(raw.Value, "${")
}
