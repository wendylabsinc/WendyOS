package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestLoadModelCatalogDefaultsToBuiltIn(t *testing.T) {
	c, err := loadModelCatalog(zap.NewNop(), "")
	if err != nil || c.Version == "" {
		t.Fatalf("catalog = %+v, %v", c, err)
	}
}

func TestLoadModelCatalogOverride(t *testing.T) {
	sha := strings.Repeat("1", 64)
	variant := func(image string) string {
		return `{"version":"dev","models":[{"id":"fake-detector","kind":"detector","labels":["person"],"variants":[{"id":"v","engine":"onnxruntime","host_image":"` +
			image + `","file":{"url":"https://example.com/m","sha256":"` + sha + `","bytes":1},"input_size":1}]}]}`
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(good, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake@sha256:"+strings.Repeat("a", 64))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake:latest")), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := loadModelCatalog(zap.NewNop(), good); err != nil || c.Version != "dev" {
		t.Fatalf("override = %+v, %v", c, err)
	}
	if _, err := loadModelCatalog(zap.NewNop(), bad); err == nil {
		t.Fatal("an override with a tag-pinned image was accepted")
	}
}
