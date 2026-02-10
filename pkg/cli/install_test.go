package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallK9sPluginWritesDropInFile(t *testing.T) {
	root := t.TempDir()

	result, err := InstallK9sPlugin(root)
	if err != nil {
		t.Fatalf("install plugin: %v", err)
	}
	if !result.Installed {
		t.Fatalf("expected install to report Installed=true")
	}

	expected := filepath.Join(root, "plugins", k9sPluginName, k9sPluginName+".yaml")
	if result.PluginFile != expected {
		t.Fatalf("unexpected plugin file path: got %s want %s", result.PluginFile, expected)
	}

	raw, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read plugin file: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	want := strings.TrimSpace(k9sPluginYAML)
	if got != want {
		t.Fatalf("unexpected plugin content:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestInstallK9sPluginIsIdempotentForSameContent(t *testing.T) {
	root := t.TempDir()

	first, err := InstallK9sPlugin(root)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !first.Installed {
		t.Fatalf("expected first install to install")
	}

	second, err := InstallK9sPlugin(root)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second.Installed {
		t.Fatalf("expected second install to be no-op")
	}
}

func TestInstallK9sPluginNoopOnConflictingExistingFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "plugins", k9sPluginName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create plugin dir: %v", err)
	}
	path := filepath.Join(dir, k9sPluginName+".yaml")
	if err := os.WriteFile(path, []byte("shortCut: X\ncommand: bad\n"), 0o644); err != nil {
		t.Fatalf("seed conflicting plugin file: %v", err)
	}

	result, err := InstallK9sPlugin(root)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.Installed {
		t.Fatalf("expected no-op when plugin file already exists")
	}
}
