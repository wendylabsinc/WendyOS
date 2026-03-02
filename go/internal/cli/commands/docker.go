package commands

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OCILayer represents a single layer extracted from an OCI/Docker tar archive.
type OCILayer struct {
	Digest   string
	DiffID   string
	Size     int64
	FilePath string // path within the tar
	GZip     bool
}

// OCIImage holds all the extracted components of a Docker/OCI image tar.
type OCIImage struct {
	Layers     []OCILayer
	Config     []byte
	Manifest   []byte
	Cmd        []string
	Entrypoint []string
	WorkingDir string
	Env        []string
}

// detectProjectType determines the project type from the directory contents.
// It checks for Dockerfile first, then language-specific markers.
func detectProjectType(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
		return "docker"
	}
	if _, err := os.Stat(filepath.Join(dir, "Package.swift")); err == nil {
		return "swift"
	}
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		return "python"
	}
	if _, err := os.Stat(filepath.Join(dir, "setup.py")); err == nil {
		return "python"
	}
	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
		return "python"
	}
	return "unknown"
}

// generatePythonDockerfile creates a Dockerfile for Python projects that do not already have one.
// It returns the path to the generated Dockerfile.
func generatePythonDockerfile(dir string) (string, error) {
	dockerfilePath := filepath.Join(dir, "Dockerfile")

	// Determine if requirements.txt exists.
	hasRequirements := false
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		hasRequirements = true
	}

	// Determine the entry point: look for app.py, main.py, or fall back.
	entryPoint := "app.py"
	for _, candidate := range []string{"app.py", "main.py"} {
		if _, err := os.Stat(filepath.Join(dir, candidate)); err == nil {
			entryPoint = candidate
			break
		}
	}

	var sb strings.Builder
	sb.WriteString("FROM python:3.11-slim\n")
	sb.WriteString("WORKDIR /app\n")
	if hasRequirements {
		sb.WriteString("COPY requirements.txt .\n")
		sb.WriteString("RUN pip install --no-cache-dir -r requirements.txt\n")
	}
	sb.WriteString("COPY . .\n")
	sb.WriteString(fmt.Sprintf("CMD [\"python\", \"%s\"]\n", entryPoint))

	if err := os.WriteFile(dockerfilePath, []byte(sb.String()), 0o644); err != nil {
		return "", fmt.Errorf("writing generated Dockerfile: %w", err)
	}

	return dockerfilePath, nil
}

// buildDockerImage builds a Docker image for the specified platform using docker buildx.
func buildDockerImage(ctx context.Context, dir, imageName, platform string, streamOutput io.Writer) error {
	args := []string{
		"buildx", "build",
		"--platform", platform,
		"-t", imageName,
		"--load",
		".",
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Stdout = streamOutput
	cmd.Stderr = streamOutput

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker buildx build failed: %w", err)
	}
	return nil
}

// saveDockerImage exports a Docker image as a tar archive.
func saveDockerImage(ctx context.Context, imageName, outputPath string) error {
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer outFile.Close()

	cmd := exec.CommandContext(ctx, "docker", "save", imageName)
	cmd.Stdout = outFile
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker save failed: %w", err)
	}
	return nil
}

// dockerManifestJSON represents the top-level manifest.json in a docker save tar.
type dockerManifestJSON struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// ociImageConfig represents the image config JSON (just the fields we need).
type ociImageConfig struct {
	Config struct {
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
		WorkingDir string   `json:"WorkingDir"`
		Env        []string `json:"Env"`
	} `json:"config"`
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// extractOCIImage parses a Docker save tar archive and extracts layer metadata,
// config JSON, and manifest JSON. Layer data is kept on disk in temporary files.
func extractOCIImage(tarPath string) (*OCIImage, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening tar: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)

	// First pass: read manifest.json, config JSON, and catalog all layer paths.
	var manifestData []byte
	var configPath string
	allFiles := make(map[string]int64) // path -> size

	// We need to store layer data, so we will do a two-pass approach.
	// First pass: collect metadata. Second pass: read layer data.
	type fileEntry struct {
		data []byte
		size int64
	}
	fileContents := make(map[string]*fileEntry)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar header: %w", err)
		}

		allFiles[hdr.Name] = hdr.Size

		// Read all files into memory for simplicity (docker save tars are manageable).
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading tar entry %s: %w", hdr.Name, err)
		}
		fileContents[hdr.Name] = &fileEntry{data: data, size: hdr.Size}
	}

	// Parse manifest.json.
	manifestEntry, ok := fileContents["manifest.json"]
	if !ok {
		return nil, fmt.Errorf("manifest.json not found in tar")
	}

	var manifests []dockerManifestJSON
	if err := json.Unmarshal(manifestEntry.data, &manifests); err != nil {
		return nil, fmt.Errorf("parsing manifest.json: %w", err)
	}
	if len(manifests) == 0 {
		return nil, fmt.Errorf("empty manifest.json")
	}

	manifest := manifests[0]
	configPath = manifest.Config
	manifestData = manifestEntry.data

	// Read config JSON.
	configEntry, ok := fileContents[configPath]
	if !ok {
		return nil, fmt.Errorf("config %s not found in tar", configPath)
	}

	// Parse config to get DiffIDs.
	var imgConfig ociImageConfig
	if err := json.Unmarshal(configEntry.data, &imgConfig); err != nil {
		return nil, fmt.Errorf("parsing image config: %w", err)
	}

	diffIDs := imgConfig.RootFS.DiffIDs

	if len(manifest.Layers) != len(diffIDs) {
		return nil, fmt.Errorf("layer count mismatch: manifest has %d, config has %d diff_ids",
			len(manifest.Layers), len(diffIDs))
	}

	// Build layer info.
	var layers []OCILayer
	for i, layerPath := range manifest.Layers {
		entry, ok := fileContents[layerPath]
		if !ok {
			return nil, fmt.Errorf("layer %s not found in tar", layerPath)
		}

		// Compute the sha256 digest of the layer data.
		h := sha256.Sum256(entry.data)
		digest := "sha256:" + hex.EncodeToString(h[:])

		// Check if the layer is gzip compressed.
		isGzip := len(entry.data) >= 2 && entry.data[0] == 0x1f && entry.data[1] == 0x8b

		// If gzip, compute the DiffID by decompressing and hashing.
		diffID := diffIDs[i]

		layers = append(layers, OCILayer{
			Digest:   digest,
			DiffID:   diffID,
			Size:     int64(len(entry.data)),
			FilePath: layerPath,
			GZip:     isGzip,
		})
	}

	return &OCIImage{
		Layers:     layers,
		Config:     configEntry.data,
		Manifest:   manifestData,
		Cmd:        imgConfig.Config.Cmd,
		Entrypoint: imgConfig.Config.Entrypoint,
		WorkingDir: imgConfig.Config.WorkingDir,
		Env:        imgConfig.Config.Env,
	}, nil
}

// readLayerData reads the raw bytes of a specific layer from the tar archive.
func readLayerData(tarPath, layerPath string) ([]byte, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening tar: %w", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("layer %s not found in tar", layerPath)
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}
		if hdr.Name == layerPath {
			return io.ReadAll(tr)
		}
	}
}

// computeGzipDiffID decompresses gzip data and returns the sha256 digest of the uncompressed content.
func computeGzipDiffID(data []byte) (string, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return "", fmt.Errorf("creating gzip reader: %w", err)
	}
	defer gz.Close()

	h := sha256.New()
	if _, err := io.Copy(h, gz); err != nil {
		return "", fmt.Errorf("hashing decompressed data: %w", err)
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
