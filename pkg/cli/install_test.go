package cli

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInstallK9sPluginAppendsWithoutOverwritingExisting(t *testing.T) {
	dir := t.TempDir()
	pluginFile := filepath.Join(dir, "plugins.yaml")
	initial := `plugins:
  existing-plugin:
    shortCut: X
    command: echo
`
	if err := os.WriteFile(pluginFile, []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial plugins.yaml: %v", err)
	}

	result, err := InstallK9sPlugin(dir)
	if err != nil {
		t.Fatalf("install plugin: %v", err)
	}
	if !result.Installed {
		t.Fatalf("expected install to report Installed=true")
	}

	cfg := readPluginsConfig(t, pluginFile)
	plugins, ok := cfg["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("expected plugins mapping")
	}
	if _, ok := plugins["existing-plugin"]; !ok {
		t.Fatalf("existing plugin was removed")
	}
	if _, ok := plugins[k9sPluginName]; !ok {
		t.Fatalf("spot interrupter plugin missing")
	}
}

func TestInstallK9sPluginIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	first, err := InstallK9sPlugin(dir)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !first.Installed {
		t.Fatalf("expected first install to install plugin")
	}

	second, err := InstallK9sPlugin(dir)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second.Installed {
		t.Fatalf("expected second install to be no-op")
	}

	cfg := readPluginsConfig(t, filepath.Join(dir, "plugins.yaml"))
	plugins, ok := cfg["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("expected plugins mapping")
	}
	count := 0
	for key := range plugins {
		if key == k9sPluginName {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected plugin entry once, got %d", count)
	}
}

func readPluginsConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read plugins config: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse plugins config: %v", err)
	}
	return out
}
