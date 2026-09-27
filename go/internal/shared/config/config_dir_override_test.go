package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigDirOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "isolated")
	t.Setenv("WENDY_CONFIG_DIR", dir)
	got, err := ConfigDir()
	if err != nil || got != dir {
		t.Fatalf("ConfigDir() = %q, %v", got, err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("isolated directory was not created: %v", err)
	}
	if err := Save(&Config{DefaultDevice: "only-this-trial"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil || loaded.DefaultDevice != "only-this-trial" {
		t.Fatalf("configuration did not stay in the isolated directory: %+v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDirOverrideRejectsRelativePath(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", "relative-directory")
	if _, err := ConfigDir(); err == nil {
		t.Fatal("relative override would make device identity depend on the working directory")
	}
}
