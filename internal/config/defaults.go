package config

import _ "embed"

// defaultConfigYAML is embedded so a service-managed start can create a usable
// configuration without access to the source tree or the network.
//
//go:embed default.yaml
var defaultConfigYAML []byte

// DefaultConfigYAML returns a fresh copy of the built-in configuration used
// when the configured file does not exist (or cannot be loaded) and the daemon
// runs under the OS service manager.
func DefaultConfigYAML() []byte {
	return append([]byte(nil), defaultConfigYAML...)
}
