package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsStateConfigAndBackupAliases(t *testing.T) {
	for _, backup := range []bool{false, true} {
		t.Run(fmt.Sprint(backup), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			statePath := path
			if backup {
				statePath += ".bak"
			}
			raw := []byte(fmt.Sprintf("state: {path: %q}\n", statePath) + minimalConfig)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".bak", []byte("backup sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "state.path") {
				t.Fatalf("state alias accepted: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("config changed")
			}
			got, err = os.ReadFile(path + ".bak")
			if err != nil || string(got) != "backup sentinel" {
				t.Fatal("backup changed")
			}
		})
	}
}
