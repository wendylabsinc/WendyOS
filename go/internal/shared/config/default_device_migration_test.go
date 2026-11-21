package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyDefaultIsReadForNoticeButNotSaved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	path := filepath.Join(dir, "config.json")
	original := `{"defaultDevice":"old.local","defaultBuildHost":"builder.local"}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LegacyDefaultDevice != "old.local" || cfg.DefaultBuildHost != "builder.local" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	before, err := os.ReadFile(path)
	if err != nil || string(before) != original {
		t.Fatal("Load mutated config")
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "defaultDevice") {
		t.Fatal("Save retained legacy default")
	}
	after, err := Load()
	if err != nil || after.LegacyDefaultDevice != "" || after.DefaultBuildHost != "builder.local" {
		t.Fatalf("bad migration: %+v, %v", after, err)
	}
}
