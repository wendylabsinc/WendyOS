package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

const (
	// defaultSwiftVersion is the Swift toolchain version used for container base images.
	defaultSwiftVersion = "6.2.3"
	// wendySDKRelease is the GitHub release tag for WendyOS Swift SDKs.
	wendySDKRelease = "0.4.0"
)

// buildSwiftContainerImage builds a Swift package and pushes the container image
// directly to the device's registry using swift-container-plugin.
func buildSwiftContainerImage(ctx context.Context, dir, product, registryHost, architecture string) error {
	sdk, err := findSwiftSDK(architecture)
	if err != nil {
		return err
	}

	swiftArgs := []string{
		"package",
		"--swift-sdk=" + sdk,
		"--allow-network-connections=all",
		"build-container-image",
		"--from=swift:" + defaultSwiftVersion + "-slim",
		"--allow-insecure-http=destination",
		"--product=" + product,
		"--repository=" + registryHost + ":5000/" + strings.ToLower(product),
		"--architecture=" + architecture,
	}

	cmd := exec.CommandContext(ctx, "swiftly", append([]string{"run", "+" + defaultSwiftVersion, "swift"}, swiftArgs...)...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("swift build-container-image failed: %w", err)
	}
	return nil
}

// findSwiftSDK looks for an installed Swift SDK for the given architecture.
// It prefers WendyOS-specific SDKs, installing one automatically if not present.
// For WASM targets (Wendy Lite), it installs the official Swift WASM SDK.
func findSwiftSDK(architecture string) (string, error) {
	// Normalize: swift-container-plugin uses "arm64" but SDKs use "aarch64".
	sdkArch := architecture
	if sdkArch == "arm64" {
		sdkArch = "aarch64"
	}

	isWasm := sdkArch == "wasm" || sdkArch == "wasm32"

	sdk, err := lookupSwiftSDK(sdkArch, isWasm)
	if err != nil {
		return "", err
	}
	if sdk != "" {
		return sdk, nil
	}

	// No suitable SDK found — install the appropriate one.
	if isWasm {
		if err := installWasmSwiftSDK(); err != nil {
			return "", err
		}
	} else {
		if err := installWendySwiftSDK(sdkArch); err != nil {
			return "", err
		}
	}

	// Look up again after install.
	sdk, err = lookupSwiftSDK(sdkArch, isWasm)
	if err != nil {
		return "", err
	}
	if sdk == "" {
		return "", fmt.Errorf("Swift SDK installed but not found; run 'swift sdk list' to verify")
	}
	return sdk, nil
}

// lookupSwiftSDK checks installed Swift SDKs for one matching the target architecture.
func lookupSwiftSDK(sdkArch string, isWasm bool) (string, error) {
	out, err := exec.Command("swiftly", "run", "+"+defaultSwiftVersion, "swift", "sdk", "list").Output()
	if err != nil {
		return "", fmt.Errorf("running 'swift sdk list': %w (is swiftly installed?)", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")

	if isWasm {
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "wasm") {
				return line, nil
			}
		}
		return "", nil
	}

	// Prefer a wendyos SDK.
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "wendyos") && strings.Contains(line, sdkArch) {
			return line, nil
		}
	}

	// Fall back to any matching linux SDK.
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, sdkArch) && strings.Contains(line, "linux") {
			return line, nil
		}
	}

	return "", nil
}

// installWendySwiftSDK downloads and installs the WendyOS Swift SDK for the given architecture.
func installWendySwiftSDK(sdkArch string) error {
	sdkName := fmt.Sprintf("%s-RELEASE_wendyos_%s", defaultSwiftVersion, sdkArch)
	url := fmt.Sprintf(
		"https://github.com/wendylabsinc/wendy-swift-tools/releases/download/%s/%s.artifactbundle.zip",
		wendySDKRelease, sdkName,
	)

	fmt.Printf("Installing WendyOS Swift SDK (%s)...\n", sdkName)

	cmd := exec.Command("swiftly", "run", "+"+defaultSwiftVersion, "swift", "sdk", "install", url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installing Swift SDK from %s: %w", url, err)
	}

	fmt.Println("Swift SDK installed.")
	return nil
}

// installWasmSwiftSDK downloads and installs the official Swift WASM SDK for Wendy Lite targets.
func installWasmSwiftSDK() error {
	sdkName := fmt.Sprintf("swift-%s-RELEASE", defaultSwiftVersion)
	url := fmt.Sprintf(
		"https://download.swift.org/swift-%s-release/wasm-sdk/%s/%s_wasm.artifactbundle.tar.gz",
		defaultSwiftVersion, sdkName, sdkName,
	)

	fmt.Printf("Installing Swift WASM SDK (%s)...\n", sdkName)

	cmd := exec.Command("swiftly", "run", "+"+defaultSwiftVersion, "swift", "sdk", "install", url, "--checksum", "394040ecd5260e68bb02f6c20aeede733b9b90702c2204e178f3e42413edad2a")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installing Swift WASM SDK from %s: %w", url, err)
	}

	fmt.Println("Swift WASM SDK installed.")
	return nil
}

// findSwiftProduct determines the executable target name from Package.swift.
// Falls back to the directory name.
func findSwiftProduct(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "Package.swift"))
	if err == nil {
		re := regexp.MustCompile(`\.executableTarget\(\s*name:\s*"([^"]+)"`)
		if m := re.FindSubmatch(data); len(m) > 1 {
			return string(m[1])
		}
	}
	return filepath.Base(dir)
}

// buildDockerImage builds a Docker image for the specified platform using docker buildx.
// If outputTar is non-empty, the image is exported directly to that tar file
// (Docker save format) instead of loading into the local Docker daemon.
func buildDockerImage(ctx context.Context, dir, imageName, platform, outputTar string, streamOutput io.Writer) error {
	args := []string{
		"buildx", "build",
		"--platform", platform,
		"-t", imageName,
	}

	if outputTar != "" {
		args = append(args, "--output", "type=docker,dest="+outputTar)
	} else {
		args = append(args, "--load")
	}

	args = append(args, ".")

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Stdout = streamOutput
	cmd.Stderr = streamOutput

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker buildx build failed: %w", err)
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

// registryHost formats a host for use in an HTTP registry URL,
// wrapping IPv6 addresses in brackets as required by RFC 3986.
func registryHost(host string, port int) string {
	if net.ParseIP(host) != nil && strings.Contains(host, ":") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// pushToRegistry pushes an extracted OCI image to an HTTP registry using
// the Docker Registry HTTP API V2.
func pushToRegistry(ctx context.Context, baseURL, repo string, ociImage *OCIImage, tarPath string, onProgress func(completed, total int)) error {
	total := len(ociImage.Layers) + 2 // layers + config + manifest
	completed := 0

	// Push each layer blob.
	for _, layer := range ociImage.Layers {
		exists, err := blobExists(ctx, baseURL, repo, layer.Digest)
		if err != nil {
			return fmt.Errorf("checking layer %s: %w", layer.Digest, err)
		}
		if !exists {
			data, err := readLayerData(tarPath, layer.FilePath)
			if err != nil {
				return fmt.Errorf("reading layer %s: %w", layer.Digest, err)
			}
			if err := pushBlob(ctx, baseURL, repo, layer.Digest, data); err != nil {
				return fmt.Errorf("pushing layer %s: %w", layer.Digest, err)
			}
		}
		completed++
		if onProgress != nil {
			onProgress(completed, total)
		}
	}

	// Push the config blob.
	configDigest := "sha256:" + sha256Hex(ociImage.Config)
	exists, err := blobExists(ctx, baseURL, repo, configDigest)
	if err != nil {
		return fmt.Errorf("checking config blob: %w", err)
	}
	if !exists {
		if err := pushBlob(ctx, baseURL, repo, configDigest, ociImage.Config); err != nil {
			return fmt.Errorf("pushing config blob: %w", err)
		}
	}
	completed++
	if onProgress != nil {
		onProgress(completed, total)
	}

	// Build and push the OCI manifest.
	manifest := buildOCIManifest(ociImage, configDigest)
	if err := pushManifest(ctx, baseURL, repo, "latest", manifest); err != nil {
		return fmt.Errorf("pushing manifest: %w", err)
	}
	completed++
	if onProgress != nil {
		onProgress(completed, total)
	}

	return nil
}

// sha256Hex returns the hex-encoded SHA-256 hash of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// blobExists checks whether a blob already exists in the registry.
func blobExists(ctx context.Context, baseURL, repo, digest string) (bool, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", baseURL, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// pushBlob uploads a blob to the registry using the two-step POST+PUT flow.
func pushBlob(ctx context.Context, baseURL, repo, digest string, data []byte) error {
	// Step 1: Start the upload.
	postURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", baseURL, repo)
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, nil)
	if err != nil {
		return err
	}
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		return fmt.Errorf("starting blob upload: %w", err)
	}
	postResp.Body.Close()

	if postResp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("unexpected status %d from upload start", postResp.StatusCode)
	}

	location := postResp.Header.Get("Location")
	if location == "" {
		return fmt.Errorf("no Location header in upload start response")
	}

	// Make the location absolute if it's relative.
	if strings.HasPrefix(location, "/") {
		location = baseURL + location
	}

	// Step 2: Complete the upload with PUT.
	sep := "?"
	if strings.Contains(location, "?") {
		sep = "&"
	}
	putURL := fmt.Sprintf("%s%sdigest=%s", location, sep, digest)

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putReq.ContentLength = int64(len(data))

	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		return fmt.Errorf("completing blob upload: %w", err)
	}
	putResp.Body.Close()

	if putResp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unexpected status %d from blob upload", putResp.StatusCode)
	}

	return nil
}

// ociManifest is a minimal OCI image manifest for the registry push.
type ociManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Config        ociManifestEntry   `json:"config"`
	Layers        []ociManifestEntry `json:"layers"`
}

type ociManifestEntry struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// buildOCIManifest constructs an OCI image manifest from the extracted image data.
func buildOCIManifest(ociImage *OCIImage, configDigest string) []byte {
	var layers []ociManifestEntry
	for _, layer := range ociImage.Layers {
		mediaType := "application/vnd.oci.image.layer.v1.tar"
		if layer.GZip {
			mediaType += "+gzip"
		}
		layers = append(layers, ociManifestEntry{
			MediaType: mediaType,
			Digest:    layer.Digest,
			Size:      layer.Size,
		})
	}

	m := ociManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: ociManifestEntry{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    configDigest,
			Size:      int64(len(ociImage.Config)),
		},
		Layers: layers,
	}

	data, _ := json.Marshal(m)
	return data
}

// pushManifest uploads the manifest to the registry for the given tag.
func pushManifest(ctx context.Context, baseURL, repo, tag string, manifest []byte) error {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", baseURL, repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(manifest))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	req.ContentLength = int64(len(manifest))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pushing manifest: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from manifest push", resp.StatusCode)
	}

	return nil
}
