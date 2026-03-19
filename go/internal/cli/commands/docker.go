package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// requireRegistryAuth checks whether the device's registry requires mTLS
// authentication and verifies the CLI has the necessary certs.
// Returns an error if the device is provisioned but no CLI certs are available.
func requireRegistryAuth(ctx context.Context, conn *grpcclient.AgentConnection) error {
	resp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return nil // can't determine provisioning status; let the push fail naturally
	}
	if _, ok := resp.GetResponse().(*agentpb.IsProvisionedResponse_Provisioned); ok {
		if loadCLICert() == nil {
			return fmt.Errorf("device is provisioned and its registry requires mTLS authentication.\nRun 'wendy auth login' to obtain client certificates before deploying")
		}
	}
	return nil
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

// BuildOption represents a detected build type in a project directory.
type BuildOption struct {
	Label string // display name shown in the picker
	Type  string // build type key: "docker", "swift", "python"
	File  string // the marker filename (e.g. "Dockerfile.production", "Package.swift")
}

// detectBuildOptions finds all buildable project markers in the given directory.
// Unlike detectProjectType, this returns ALL options rather than the first match,
// including multiple Dockerfiles (Dockerfile, Dockerfile.*, Dockerfile-*).
func detectBuildOptions(dir string) []BuildOption {
	var options []BuildOption

	// Find all Dockerfiles.
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.") || strings.HasPrefix(name, "Dockerfile-") {
				options = append(options, BuildOption{
					Label: name,
					Type:  "docker",
					File:  name,
				})
			}
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "Package.swift")); err == nil {
		options = append(options, BuildOption{
			Label: "Package.swift (Swift)",
			Type:  "swift",
			File:  "Package.swift",
		})
	}

	// Python — only add once even if multiple markers exist.
	for _, marker := range []string{"requirements.txt", "pyproject.toml", "setup.py"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			options = append(options, BuildOption{
				Label: marker + " (Python)",
				Type:  "python",
				File:  marker,
			})
			break
		}
	}

	return options
}

// injectDebugpy builds a wrapper image on top of the given image that installs debugpy.
func injectDebugpy(ctx context.Context, registryAddr, registryImage, platform string, streamOutput *os.File, useMTLS bool) error {
	tmpDir, err := os.MkdirTemp("", "wendy-debugpy-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dockerfile := fmt.Sprintf("FROM %s\nUSER root\nRUN pip install debugpy\n", registryImage)
	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("writing debugpy Dockerfile: %w", err)
	}

	return buildAndPushImage(ctx, tmpDir, registryAddr, registryImage, platform, streamOutput, useMTLS)
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

// wendySDKChecksums maps architecture to the checksum for the WendyOS Swift SDK bundle.
var wendySDKChecksums = map[string]string{
	"x86_64":  "b5a4d08ad4d4841043727f6671c6aa004da3a2b7f12dc28101d6770c1dc57eb1",
	"aarch64": "ef8fa5a2eda766e3b1df791dc175bbf87f570b9cc6f95ada1fe7643a327e087e",
}

// execCommandContext is the function used to create exec commands.
// It can be overridden in tests.
var execCommandContext = exec.CommandContext

// ensureSwiftVersion makes sure the required Swift toolchain is installed via swiftly.
// If the version is already present this is a no-op.
func ensureSwiftVersion(ctx context.Context) error {
	// Avoid starting any subprocesses if the context is already canceled.
	if err := ctx.Err(); err != nil {
		return err
	}

	// First, check whether the requested Swift toolchain is already installed.
	checkCmd := execCommandContext(ctx, "swiftly", "which", defaultSwiftVersion)
	// Discard output for the existence check; this is only used as a probe.
	checkCmd.Stdout = io.Discard
	checkCmd.Stderr = io.Discard
	if err := checkCmd.Run(); err == nil {
		// Toolchain is already installed; nothing to do.
		return nil
	} else {
		// If swiftly itself is missing, surface a helpful error.
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("swiftly is required but not installed; see https://swiftlang.github.io/swiftly for installation instructions")
		}
		// If the context was canceled or expired during the check, propagate that rather than starting an install.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// For other errors (e.g., version not installed), fall through and attempt installation.
	}

	// Re-check the context before starting installation.
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := execCommandContext(ctx, "swiftly", "install", defaultSwiftVersion)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("swiftly is required but not installed; see https://swiftlang.github.io/swiftly for installation instructions")
		}
		return fmt.Errorf("installing Swift %s via swiftly: %w", defaultSwiftVersion, err)
	}
	return nil
}

// buildSwiftContainerImage builds a Swift package and pushes the container image
// directly to the device's registry using swift-container-plugin.
func buildSwiftContainerImage(ctx context.Context, dir, product, host, architecture string, useMTLS bool) error {
	if err := ensureContainerPlugin(dir); err != nil {
		return err
	}

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
		"--product=" + product,
		"--repository=" + registryHost(host, 5000) + "/" + strings.ToLower(product),
		"--architecture=" + architecture,
	}

	// Use insecure HTTP when the connection is not mTLS; the registry only
	// speaks TLS when the device is provisioned and the CLI connected via mTLS.
	if !useMTLS {
		swiftArgs = append(swiftArgs, "--allow-insecure-http=destination")
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

const containerPluginMinVersion = "1.3.0"

// ensureContainerPlugin checks that swift-container-plugin is available as a
// package plugin in the given project directory. If not, it automatically adds
// the dependency using `swift package add-dependency`.
func ensureContainerPlugin(dir string) error {
	cmd := exec.Command("swiftly", "run", "+"+defaultSwiftVersion, "swift", "package", "plugin", "--list")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list Swift package plugins: %w", err)
	}

	if strings.Contains(string(out), "build-container-image") {
		return nil
	}

	fmt.Println("Adding swift-container-plugin dependency...")
	add := exec.Command("swiftly", "run", "+"+defaultSwiftVersion, "swift", "package", "add-dependency",
		"https://github.com/apple/swift-container-plugin", "--from", containerPluginMinVersion)
	add.Dir = dir
	add.Stdout = os.Stdout
	add.Stderr = os.Stderr
	if err := add.Run(); err != nil {
		return fmt.Errorf("failed to add swift-container-plugin dependency: %w", err)
	}

	return nil
}

// findSwiftSDK looks for an installed Swift SDK for the given architecture.
// It prefers WendyOS-specific SDKs, installing one automatically if not present.
// For WASM targets (Wendy Lite), it installs the official Swift WASM SDK.
func findSwiftSDK(architecture string) (string, error) {
	// Normalize: swift-container-plugin uses "arm64"/"amd64" but SDKs use "aarch64"/"x86_64".
	sdkArch := architecture
	switch sdkArch {
	case "arm64":
		sdkArch = "aarch64"
	case "amd64":
		sdkArch = "x86_64"
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

	// Prefer a wendyos SDK matching the current Swift version.
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "wendyos") && strings.Contains(line, sdkArch) && strings.Contains(line, defaultSwiftVersion) {
			return line, nil
		}
	}

	// Fall back to any matching linux SDK for the current Swift version.
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, sdkArch) && strings.Contains(line, "linux") && strings.Contains(line, defaultSwiftVersion) {
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

	checksum, ok := wendySDKChecksums[sdkArch]
	if !ok {
		return fmt.Errorf("no checksum available for architecture %s", sdkArch)
	}

	cmd := exec.Command("swiftly", "run", "+"+defaultSwiftVersion, "swift", "sdk", "install", url, "--checksum", checksum)
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

// findSwiftProduct determines the product name from Package.swift using
// `swift package dump-package`. Returns an error with a suggestion when
// no executable product can be determined.
func findSwiftProduct(dir string) (string, error) {
	cmd := exec.Command("swiftly", "run", "+"+defaultSwiftVersion, "swift", "package", "dump-package")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swift package dump-package failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var manifest struct {
		Products []struct {
			Name string `json:"name"`
		} `json:"products"`
		Targets []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(out, &manifest); err != nil {
		return "", fmt.Errorf("could not parse Package.swift manifest: %w", err)
	}

	if len(manifest.Products) == 1 {
		return manifest.Products[0].Name, nil
	}
	if len(manifest.Products) > 1 {
		var productNames []string
		for _, p := range manifest.Products {
			productNames = append(productNames, p.Name)
		}
		return "", fmt.Errorf("Package.swift declares multiple products (%s); wendy run requires a single executable product", strings.Join(productNames, ", "))
	}

	// No products — look for executable targets.
	var execTargets []string
	for _, t := range manifest.Targets {
		if t.Type == "executable" {
			execTargets = append(execTargets, t.Name)
		}
	}
	if len(execTargets) == 1 {
		return execTargets[0], nil
	}
	if len(execTargets) > 1 {
		return "", fmt.Errorf("Package.swift has multiple executable targets but no products; add an executable product for the target you want to run")
	}
	return "", fmt.Errorf("Package.swift has no executable targets or products")
}

// ensureBuildxBuilder ensures a buildx builder with the docker-container driver
// exists and returns its name plus the effective registry address to use in
// image references. For IPv6 addresses, a hostname alias is configured inside
// the builder container to avoid brackets that break the TOML parser.
func ensureBuildxBuilder(ctx context.Context, registryAddr string, useMTLS bool) (builderName, effectiveAddr string, err error) {
	// Use separate builders for mTLS and plaintext so switching between
	// provisioned and unprovisioned devices doesn't recreate builders.
	const containerCertDir = "/etc/buildkit/certs"

	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("finding home directory: %w", err)
	}
	configDir := filepath.Join(home, ".cache", "wendy")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", "", fmt.Errorf("creating config directory: %w", err)
	}

	// For IPv6 addresses (contain brackets), use a hostname alias to avoid
	// ']' in the TOML config — the go-toml v1 parser used by both docker
	// buildx and buildkitd rejects ']' in table-header keys.
	effectiveAddr, ipv6IP := splitIPv6RegistryAddr(registryAddr)

	if !useMTLS {
		builderName, err = ensurePlaintextBuilder(ctx, configDir, effectiveAddr)
	} else {
		builderName, err = ensureMTLSBuilder(ctx, configDir, effectiveAddr, containerCertDir)
	}
	if err != nil {
		return "", "", err
	}

	// Add a /etc/hosts entry inside the builder container so it can resolve
	// the alias to the real IPv6 address.
	if ipv6IP != "" {
		containerName := "buildx_buildkit_" + builderName + "0"
		hostsCmd := exec.CommandContext(ctx, "docker", "exec", containerName, "sh", "-c",
			fmt.Sprintf("if grep -q ' wendy-registry' /etc/hosts; then sed -i 's/^[^#]* wendy-registry$/%s wendy-registry/' /etc/hosts; else printf '\\n%s wendy-registry\\n' >> /etc/hosts; fi", ipv6IP, ipv6IP))
		if out, cmdErr := hostsCmd.CombinedOutput(); cmdErr != nil {
			return "", "", fmt.Errorf("adding hosts entry to builder: %s: %w", string(out), cmdErr)
		}
	}

	return builderName, effectiveAddr, nil
}

// buildkitRegistryConfig generates a buildkitd.toml snippet for the given
// registry address. IPv6 addresses must be passed through the hostname alias
// (e.g. "wendy-registry:5000") rather than in bracket notation, because the
// go-toml v1 parser used by buildkitd rejects ']' in table-header keys.
func buildkitRegistryConfig(registryAddr string, plainHTTP bool, keypair *[2]string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[registry.\"%s\"]\n", registryAddr)
	if plainHTTP {
		sb.WriteString("  http = true\n")
	}
	sb.WriteString("  insecure = true\n")
	if keypair != nil {
		fmt.Fprintf(&sb, "  [[registry.\"%s\".keypair]]\n", registryAddr)
		fmt.Fprintf(&sb, "    key = %q\n", keypair[0])
		fmt.Fprintf(&sb, "    cert = %q\n", keypair[1])
	}
	return sb.String()
}

// removeBuilder removes a buildx builder, falling back to deleting the
// instance file directly when `docker buildx rm` fails (e.g. because the
// stored config contains IPv6 brackets that the host TOML parser rejects).
func removeBuilder(ctx context.Context, name string) {
	rmCmd := exec.CommandContext(ctx, "docker", "buildx", "rm", name)
	if rmCmd.Run() == nil {
		return
	}
	// Fallback: remove the instance file and kill the container directly.
	home, err := os.UserHomeDir()
	if err == nil {
		os.Remove(filepath.Join(home, ".docker", "buildx", "instances", name))
		os.Remove(filepath.Join(home, ".docker", "buildx", "activity", name))
	}
	exec.CommandContext(ctx, "docker", "rm", "-f", "buildx_buildkit_"+name+"0").Run()
}

// ensurePlaintextBuilder ensures the "wendy" buildx builder exists with plain
// HTTP registry config. The config is injected into the builder container via
// docker cp (not --buildkitd-config) to avoid the host-side TOML parser which
// cannot handle IPv6 brackets in registry addresses.
func ensurePlaintextBuilder(ctx context.Context, configDir, registryAddr string) (string, error) {
	const builderName = "wendy"

	appliedPath := filepath.Join(configDir, "buildkitd.applied")

	fullConfig := buildkitRegistryConfig(registryAddr, true, nil)

	appliedConfig, _ := os.ReadFile(appliedPath)
	configChanged := string(appliedConfig) != fullConfig

	cmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", builderName)
	builderExists := cmd.Run() == nil

	if builderExists && configChanged {
		removeBuilder(ctx, builderName)
		builderExists = false
	}

	if !builderExists {
		cmd = exec.CommandContext(ctx, "docker", "buildx", "create",
			"--name", builderName,
			"--driver", "docker-container",
			"--driver-opt", "network=host",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("creating buildx builder %q: %s: %w", builderName, string(out), err)
		}
	}

	// Inject the real config into the builder container and restart.
	if err := updateBuilderConfig(ctx, builderName, fullConfig); err != nil {
		return "", fmt.Errorf("updating builder config: %w", err)
	}

	_ = os.WriteFile(appliedPath, []byte(fullConfig), 0o644)
	return builderName, nil
}

// ensureMTLSBuilder ensures the "wendy-mtls" buildx builder exists with mTLS
// client certs for the device registry.
func ensureMTLSBuilder(ctx context.Context, configDir, registryAddr, containerCertDir string) (string, error) {
	const builderName = "wendy-mtls"

	appliedPath := filepath.Join(configDir, "buildkitd-mtls.applied")

	certInfo := loadCLICert()
	if certInfo == nil || certInfo.PemCertificate == "" || certInfo.PemPrivateKey == "" {
		return "", fmt.Errorf("mTLS connection but no CLI certificates available")
	}

	// Write cert files to host; they'll be docker-cp'd into the builder container.
	hostCertDir := filepath.Join(configDir, "certs")
	if err := os.MkdirAll(hostCertDir, 0o700); err != nil {
		return "", fmt.Errorf("creating cert directory: %w", err)
	}

	certPath := filepath.Join(hostCertDir, "client-cert.pem")
	keyPath := filepath.Join(hostCertDir, "client-key.pem")
	caPath := filepath.Join(hostCertDir, "ca.pem")

	fullCert := certInfo.PemCertificate
	if certInfo.PemCertificateChain != "" {
		fullCert += "\n" + certInfo.PemCertificateChain
	}
	if err := os.WriteFile(certPath, []byte(fullCert), 0o644); err != nil {
		return "", fmt.Errorf("writing client cert: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(certInfo.PemPrivateKey), 0o600); err != nil {
		return "", fmt.Errorf("writing client key: %w", err)
	}
	if certInfo.PemCertificateChain != "" {
		if err := os.WriteFile(caPath, []byte(certInfo.PemCertificateChain), 0o644); err != nil {
			return "", fmt.Errorf("writing CA cert: %w", err)
		}
	}

	keypair := &[2]string{containerCertDir + "/client-key.pem", containerCertDir + "/client-cert.pem"}
	fullConfig := buildkitRegistryConfig(registryAddr, false, keypair)

	appliedConfig, _ := os.ReadFile(appliedPath)
	configChanged := string(appliedConfig) != fullConfig

	cmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", builderName)
	builderExists := cmd.Run() == nil

	if builderExists && configChanged {
		removeBuilder(ctx, builderName)
		builderExists = false
	}

	if !builderExists {
		cmd = exec.CommandContext(ctx, "docker", "buildx", "create",
			"--name", builderName,
			"--driver", "docker-container",
			"--driver-opt", "network=host",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("creating buildx builder %q: %s: %w", builderName, string(out), err)
		}
	}

	// Copy mTLS certs into the builder container and update buildkitd config.
	if err := copyCertsToBuilder(ctx, builderName, hostCertDir, containerCertDir); err != nil {
		return "", fmt.Errorf("copying certs to builder: %w", err)
	}
	if err := updateBuilderConfig(ctx, builderName, fullConfig); err != nil {
		return "", fmt.Errorf("updating builder config: %w", err)
	}

	_ = os.WriteFile(appliedPath, []byte(fullConfig), 0o644)
	return builderName, nil
}

// copyCertsToBuilder bootstraps the buildx builder container and copies TLS
// client certificates from the host into it so buildkitd can authenticate
// with the device registry.
func copyCertsToBuilder(ctx context.Context, builderName, hostCertDir, containerCertDir string) error {
	// Bootstrap the builder to ensure the container is running.
	cmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
	}

	// The docker-container driver names the container buildx_buildkit_<name>0.
	containerName := "buildx_buildkit_" + builderName + "0"

	// Copy cert files into the running container.
	cmd = exec.CommandContext(ctx, "docker", "cp", hostCertDir+"/.", containerName+":"+containerCertDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp certs: %s: %w", string(out), err)
	}

	return nil
}

// updateBuilderConfig bootstraps the buildx builder container (if not already
// running), writes a new buildkitd.toml into it, and restarts so the updated
// configuration takes effect.
func updateBuilderConfig(ctx context.Context, builderName, config string) error {
	// Bootstrap the builder to ensure the container is running.
	bootstrapCmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	if out, err := bootstrapCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
	}

	containerName := "buildx_buildkit_" + builderName + "0"
	const containerConfigPath = "/etc/buildkit/buildkitd.toml"

	// Write config to a temp file, then docker-cp it in.
	tmp, err := os.CreateTemp("", "buildkitd-*.toml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(config); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp config: %w", err)
	}
	tmp.Close()

	cmd := exec.CommandContext(ctx, "docker", "cp", tmp.Name(), containerName+":"+containerConfigPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp config: %s: %w", string(out), err)
	}

	// Restart the container so buildkitd reloads the config.
	cmd = exec.CommandContext(ctx, "docker", "restart", containerName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restarting builder: %s: %w", string(out), err)
	}

	return nil
}

// buildAndPushImage builds a Docker image for the specified platform and pushes
// it directly to the given registry using docker buildx. The registry transport
// is conditional: plain HTTP for plaintext devices, and TLS/mTLS for provisioned
// devices when useMTLS is enabled.
func buildAndPushImage(ctx context.Context, dir, registryAddr, registryImage, platform string, streamOutput *os.File, useMTLS bool) error {
	builder, effectiveAddr, err := ensureBuildxBuilder(ctx, registryAddr, useMTLS)
	if err != nil {
		return err
	}

	// When an IPv6 alias is in use, rewrite the image reference to match.
	if effectiveAddr != registryAddr {
		registryImage = strings.Replace(registryImage, registryAddr, effectiveAddr, 1)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home directory: %w", err)
	}
	cacheDir := filepath.Join(home, ".cache", "wendy", "buildx")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("creating cache directory: %w", err)
	}

	args := []string{
		"buildx", "build",
		"--builder", builder,
		"--platform", platform,
		"--cache-from", "type=local,src=" + cacheDir,
		"--cache-to", "type=local,dest=" + cacheDir,
		"--output", "type=image,name=" + registryImage + ",push=true",
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

// registryHost formats a host:port for use in a registry image reference,
// wrapping IPv6 addresses in brackets as required by RFC 3986.
// If the host is a hostname (not an IP), it is resolved to an IP address first
// so that Docker buildx (which runs inside a VM with its own DNS) can reach the
// device registry even when the hostname is only resolvable via mDNS or
// Tailscale DNS on the host machine.
//
// IPv6 link-local addresses (fe80::/10) contain a zone ID (e.g. %en0) that is
// meaningful only on the host machine and cannot be used inside a Docker
// buildkit container. For literal IP inputs the zone ID is stripped; for
// hostnames the resolver prefers routable IPv4 or global IPv6 addresses.
func registryHost(host string, port int) string {
	host = resolveRegistryIP(host)
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// splitIPv6RegistryAddr checks if registryAddr is a bracketed IPv6 address
// (e.g. "[fe80::1%en0]:5000") and, if so, returns a hostname alias
// ("wendy-registry:<port>") as the effective address and the bare IPv6 IP
// (zone stripped) for use in /etc/hosts. For non-IPv6 addresses, the input
// is returned unchanged and ipv6IP is empty.
func splitIPv6RegistryAddr(registryAddr string) (effectiveAddr, ipv6IP string) {
	idx := strings.Index(registryAddr, "]:")
	if idx == -1 {
		return registryAddr, ""
	}
	raw := registryAddr[1:idx]
	port := registryAddr[idx+2:]
	if addr, err := netip.ParseAddr(raw); err == nil {
		ipv6IP = addr.WithZone("").String()
	} else {
		ipv6IP = raw
	}
	return "wendy-registry:" + port, ipv6IP
}

// resolveRegistryIP resolves a host string to an IP address suitable for use
// inside a Docker buildkit container. It prefers routable addresses but may
// fall back to a zone-less link-local IPv6 address as a last resort.
//
// It handles three cases:
//  1. Hostname — resolved via DNS, preferring IPv4 over IPv6 link-local.
//  2. IPv6 with zone ID (fe80::…%en0) — detected via netip.ParseAddr and
//     returned with the zone stripped (zones are host-specific and don't
//     exist inside the builder container's network namespace).
//  3. Any other IP — returned as-is.
func resolveRegistryIP(host string) string {
	// netip.ParseAddr handles zone IDs; net.ParseIP does not.
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.WithZone("").String()
	}

	// Not a bare IP — treat as hostname and resolve.
	if net.ParseIP(host) == nil {
		if resolved := resolveHostPreferRoutable(host); resolved != "" {
			return resolved
		}
	}
	return host
}

// resolveHostPreferRoutable resolves a hostname and returns the best address
// for use inside a Docker container. It prefers, in order:
//  1. IPv4 addresses (from DNS)
//  2. Global/ULA IPv6 addresses
//  3. IPv4 discovered via ARP/NDP correlation (when DNS only returns link-local IPv6)
//  4. Link-local IPv6 (stripped of zone ID, as a last resort)
func resolveHostPreferRoutable(hostname string) string {
	addrs, err := net.LookupHost(hostname)
	if err != nil || len(addrs) == 0 {
		return ""
	}

	var fallbackLinkLocal string
	for _, a := range addrs {
		addr, parseErr := netip.ParseAddr(a)
		if parseErr != nil {
			continue
		}
		if addr.Is4() {
			return a // IPv4 is always preferred
		}
		if !addr.IsLinkLocalUnicast() {
			return addr.WithZone("").String() // global/ULA IPv6
		}
		if fallbackLinkLocal == "" {
			fallbackLinkLocal = addr.WithZone("").String()
		}
	}

	// DNS returned only link-local IPv6 — this is unroutable from Docker
	// containers (zone IDs are host-specific). Try to find the device's
	// IPv4 address by correlating MAC addresses between the NDP and ARP
	// neighbor tables. This is common for USB-connected devices where
	// mDNS only advertises an AAAA record but the device also has an
	// IPv4 link-local address (169.254.x.x).
	if fallbackLinkLocal != "" {
		if ipv4 := findIPv4ViaNeighborTable(fallbackLinkLocal); ipv4 != "" {
			return ipv4
		}
	}

	return fallbackLinkLocal // link-local without zone as last resort
}

// findIPv4ViaNeighborTable tries to find the IPv4 address of a device known
// by its IPv6 link-local address. It correlates MAC addresses between the
// IPv6 neighbor table (NDP) and the IPv4 neighbor table (ARP).
// Returns "" if no IPv4 address can be found.
func findIPv4ViaNeighborTable(ipv6LinkLocal string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		return findIPv4NeighborDarwin(ctx, ipv6LinkLocal)
	case "linux":
		return findIPv4NeighborLinux(ctx, ipv6LinkLocal)
	default:
		return ""
	}
}

// findIPv4NeighborDarwin looks up the IPv4 address for a device on macOS.
// It finds the device's MAC address from the NDP table, then searches
// the ARP table for an IPv4 entry with the same MAC on the same interface.
func findIPv4NeighborDarwin(ctx context.Context, ipv6LinkLocal string) string {
	// Step 1: Find MAC address and interface from NDP table.
	// ndp -an output: "fe80::1%en6  aa:bb:cc:dd:ee:ff  en6  23h49m  S  R"
	ndpOut, err := exec.CommandContext(ctx, "ndp", "-an").Output()
	if err != nil {
		return ""
	}

	var mac, iface string
	for _, line := range strings.Split(string(ndpOut), "\n") {
		if !strings.Contains(line, ipv6LinkLocal) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			mac = fields[1]
			iface = fields[2]
		}
		break
	}
	if mac == "" || mac == "(incomplete)" || iface == "" {
		return ""
	}

	// Step 2: Find IPv4 with the same MAC in ARP table on the same interface.
	// arp -an -i en6 output: "? (169.254.189.250) at aa:bb:cc:dd:ee:ff on en6 ..."
	arpOut, err := exec.CommandContext(ctx, "arp", "-an", "-i", iface).Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(arpOut), "\n") {
		if !strings.Contains(line, mac) {
			continue
		}
		start := strings.Index(line, "(")
		end := strings.Index(line, ")")
		if start >= 0 && end > start {
			ip := line[start+1 : end]
			if parsed, parseErr := netip.ParseAddr(ip); parseErr == nil && parsed.Is4() {
				return ip
			}
		}
	}

	return ""
}

// findIPv4NeighborLinux looks up the IPv4 address for a device on Linux.
// It finds the device's MAC and interface from the IPv6 neighbor table,
// then searches the IPv4 neighbor table for a matching MAC.
func findIPv4NeighborLinux(ctx context.Context, ipv6LinkLocal string) string {
	// Step 1: Find MAC and interface from ip -6 neigh.
	// Output: "fe80::1 dev eth0 lladdr aa:bb:cc:dd:ee:ff STALE"
	neighOut, err := exec.CommandContext(ctx, "ip", "-6", "neigh", "show").Output()
	if err != nil {
		return ""
	}

	var mac, iface string
	for _, line := range strings.Split(string(neighOut), "\n") {
		if !strings.Contains(line, ipv6LinkLocal) {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "lladdr" && i+1 < len(fields) {
				mac = fields[i+1]
			}
			if f == "dev" && i+1 < len(fields) {
				iface = fields[i+1]
			}
		}
		break
	}
	if mac == "" || iface == "" {
		return ""
	}

	// Step 2: Find IPv4 with same MAC on the same interface.
	// Output: "169.254.189.250 dev usb0 lladdr aa:bb:cc:dd:ee:ff REACHABLE"
	arpOut, err := exec.CommandContext(ctx, "ip", "-4", "neigh", "show", "dev", iface).Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(arpOut), "\n") {
		if !strings.Contains(line, mac) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			if parsed, parseErr := netip.ParseAddr(fields[0]); parseErr == nil && parsed.Is4() {
				return fields[0]
			}
		}
	}

	return ""
}

// buildSwiftDockerImage cross-compiles a Swift package for Linux and builds a
// Docker image containing the resulting binary. Returns the Docker image name.
// This is used by the Docker Desktop provider for Swift projects that do not
// have a Dockerfile, as an alternative to swift-container-plugin (which only
// supports pushing to registries).
func buildSwiftDockerImage(ctx context.Context, dir, product string) (string, error) {
	arch := runtime.GOARCH
	sdk, err := findSwiftSDK(arch)
	if err != nil {
		return "", fmt.Errorf("finding Swift SDK: %w", err)
	}

	cliLogln("Cross-compiling %s for linux/%s...", product, arch)
	buildCmd := exec.CommandContext(ctx, "swiftly", "run", "+"+defaultSwiftVersion, "swift",
		"build", "-c", "release", "--swift-sdk="+sdk, "--product", product)
	buildCmd.Dir = dir
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return "", fmt.Errorf("swift build: %w", err)
	}

	// Determine the binary output path.
	showBinCmd := exec.CommandContext(ctx, "swiftly", "run", "+"+defaultSwiftVersion, "swift",
		"build", "-c", "release", "--swift-sdk="+sdk, "--show-bin-path")
	showBinCmd.Dir = dir
	out, err := showBinCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swift build --show-bin-path: %w\n%s", err, string(out))
	}
	binDir := strings.TrimSpace(string(out))
	srcBin := filepath.Join(binDir, product)

	// Create a temp directory with the binary and a minimal Dockerfile.
	tmpDir, err := os.MkdirTemp("", "wendy-swift-docker-*")
	if err != nil {
		return "", fmt.Errorf("creating temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Copy the cross-compiled binary to a fixed name to avoid Dockerfile
	// issues with special characters in Swift product names.
	dstBin := filepath.Join(tmpDir, "app")
	if err := copyBinary(srcBin, dstBin); err != nil {
		return "", fmt.Errorf("copying binary: %w", err)
	}

	// Write a minimal Dockerfile using the fixed binary name.
	dockerfile := fmt.Sprintf("FROM swift:%s-slim\nCOPY app /usr/local/bin/app\nCMD [\"app\"]\n",
		defaultSwiftVersion)
	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", fmt.Errorf("writing Dockerfile: %w", err)
	}

	// Build the Docker image with a sanitised name.
	imageName := sanitizeDockerImageName(product) + ":latest"
	dockerCmd := exec.CommandContext(ctx, "docker", "build", "-t", imageName, ".")
	dockerCmd.Dir = tmpDir
	dockerCmd.Stdout = os.Stdout
	dockerCmd.Stderr = os.Stderr
	if err := dockerCmd.Run(); err != nil {
		return "", fmt.Errorf("docker build: %w", err)
	}

	return imageName, nil
}

// sanitizeDockerImageName produces a valid Docker image reference component
// from an arbitrary string (e.g. a Swift product name).
func sanitizeDockerImageName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	result := strings.Trim(b.String(), "-.")
	if result == "" {
		return "wendy-app"
	}
	return result
}

// copyBinary copies a file from src to dst with mode 0755.
func copyBinary(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	mode := srcInfo.Mode().Perm() | 0o111

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
