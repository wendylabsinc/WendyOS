package commands

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectProjectType_Dockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "docker" {
		t.Errorf("detectProjectType = %q; want %q", got, "docker")
	}
}

func TestDetectProjectType_PackageSwift(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// swift"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "swift" {
		t.Errorf("detectProjectType = %q; want %q", got, "swift")
	}
}

func TestDetectProjectType_RequirementsTxt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_SetupPy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("setup()"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_PyprojectToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[tool.poetry]"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := detectProjectType(dir)
	if got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_Unknown(t *testing.T) {
	dir := t.TempDir()
	got := detectProjectType(dir)
	if got != "unknown" {
		t.Errorf("detectProjectType = %q; want %q", got, "unknown")
	}
}

func TestDetectProjectType_DockerfileTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	// Create both Dockerfile and requirements.txt; Dockerfile should win.
	for _, name := range []string{"Dockerfile", "requirements.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := detectProjectType(dir)
	if got != "docker" {
		t.Errorf("detectProjectType = %q; want %q (Dockerfile should take precedence)", got, "docker")
	}
}

func TestGeneratePythonDockerfile_WithRequirements(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated Dockerfile: %v", err)
	}
	content := string(data)

	expectations := []string{
		"FROM python:3.11-slim",
		"WORKDIR /app",
		"COPY requirements.txt .",
		"RUN pip install --no-cache-dir -r requirements.txt",
		"COPY . .",
		`CMD ["python", "app.py"]`,
	}
	for _, exp := range expectations {
		if !strings.Contains(content, exp) {
			t.Errorf("generated Dockerfile missing %q\nGot:\n%s", exp, content)
		}
	}
}

func TestGeneratePythonDockerfile_WithoutRequirements_MainPy(t *testing.T) {
	dir := t.TempDir()
	// Only main.py, no requirements.txt, no app.py.
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated Dockerfile: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "requirements.txt") {
		t.Error("Dockerfile should not mention requirements.txt when it does not exist")
	}
	if !strings.Contains(content, `CMD ["python", "main.py"]`) {
		t.Errorf("expected CMD with main.py, got:\n%s", content)
	}
}

func TestGeneratePythonDockerfile_FallbackEntrypoint(t *testing.T) {
	dir := t.TempDir()
	// No app.py or main.py; should fall back to app.py as default.

	_, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `CMD ["python", "app.py"]`) {
		t.Errorf("expected fallback to app.py entrypoint, got:\n%s", string(data))
	}
}

// buildTestDockerSaveTar creates a minimal Docker save tar in memory and writes it to disk.
func buildTestDockerSaveTar(t *testing.T, destPath string) {
	t.Helper()

	// Create a fake layer (plain tar, not gzip).
	layerContent := []byte("fake-layer-content-for-testing")
	h := sha256.Sum256(layerContent)
	layerDigest := "sha256:" + hex.EncodeToString(h[:])

	// The diffID for an uncompressed layer equals its sha256 digest.
	diffID := layerDigest

	configJSON := map[string]interface{}{
		"rootfs": map[string]interface{}{
			"diff_ids": []string{diffID},
		},
	}
	configData, err := json.Marshal(configJSON)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}

	configHash := sha256.Sum256(configData)
	configName := hex.EncodeToString(configHash[:]) + ".json"

	manifestJSON := []map[string]interface{}{
		{
			"Config":   configName,
			"RepoTags": []string{"test:latest"},
			"Layers":   []string{"layer.tar"},
		},
	}
	manifestData, err := json.Marshal(manifestJSON)
	if err != nil {
		t.Fatalf("marshaling manifest: %v", err)
	}

	// Build the tar archive.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	entries := []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifestData},
		{configName, configData},
		{"layer.tar", layerContent},
	}

	for _, e := range entries {
		hdr := &tar.Header{
			Name: e.name,
			Size: int64(len(e.data)),
			Mode: 0o644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", e.name, err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatalf("writing tar data for %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}

	if err := os.WriteFile(destPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing test tar: %v", err)
	}
}

func TestExtractOCIImage(t *testing.T) {
	tarPath := filepath.Join(t.TempDir(), "test-image.tar")
	buildTestDockerSaveTar(t, tarPath)

	img, err := extractOCIImage(tarPath)
	if err != nil {
		t.Fatalf("extractOCIImage: %v", err)
	}

	if img == nil {
		t.Fatal("extractOCIImage returned nil")
	}

	if len(img.Layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(img.Layers))
	}

	layer := img.Layers[0]
	if layer.FilePath != "layer.tar" {
		t.Errorf("layer FilePath = %q; want %q", layer.FilePath, "layer.tar")
	}
	if !strings.HasPrefix(layer.Digest, "sha256:") {
		t.Errorf("layer Digest should start with sha256:, got %q", layer.Digest)
	}
	if !strings.HasPrefix(layer.DiffID, "sha256:") {
		t.Errorf("layer DiffID should start with sha256:, got %q", layer.DiffID)
	}
	if layer.GZip {
		t.Error("layer should not be detected as gzip (plain content)")
	}
	if layer.Size <= 0 {
		t.Errorf("layer Size = %d; want > 0", layer.Size)
	}

	if len(img.Config) == 0 {
		t.Error("Config should not be empty")
	}
	if len(img.Manifest) == 0 {
		t.Error("Manifest should not be empty")
	}
}

func TestExtractOCIImage_MissingManifest(t *testing.T) {
	// Build a tar with no manifest.json.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "something.txt", Size: 5, Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("hello"))
	tw.Close()

	tarPath := filepath.Join(t.TempDir(), "bad.tar")
	os.WriteFile(tarPath, buf.Bytes(), 0o644)

	_, err := extractOCIImage(tarPath)
	if err == nil {
		t.Fatal("expected error for tar without manifest.json")
	}
	if !strings.Contains(err.Error(), "manifest.json") {
		t.Errorf("error should mention manifest.json, got: %v", err)
	}
}

func TestReadLayerData(t *testing.T) {
	tarPath := filepath.Join(t.TempDir(), "test-image.tar")
	buildTestDockerSaveTar(t, tarPath)

	data, err := readLayerData(tarPath, "layer.tar")
	if err != nil {
		t.Fatalf("readLayerData: %v", err)
	}

	expected := "fake-layer-content-for-testing"
	if string(data) != expected {
		t.Errorf("readLayerData = %q; want %q", string(data), expected)
	}
}

func TestReadLayerData_NotFound(t *testing.T) {
	tarPath := filepath.Join(t.TempDir(), "test-image.tar")
	buildTestDockerSaveTar(t, tarPath)

	_, err := readLayerData(tarPath, "nonexistent.tar")
	if err == nil {
		t.Fatal("expected error for missing layer")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}
