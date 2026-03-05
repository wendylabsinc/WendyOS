package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

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

// wendySDKChecksums maps architecture to the checksum for the WendyOS Swift SDK bundle.
var wendySDKChecksums = map[string]string{
	"x86_64":  "b5a4d08ad4d4841043727f6671c6aa004da3a2b7f12dc28101d6770c1dc57eb1",
	"aarch64": "ef8fa5a2eda766e3b1df791dc175bbf87f570b9cc6f95ada1fe7643a327e087e",
}

// buildSwiftContainerImage builds a Swift package and pushes the container image
// directly to the device's registry using swift-container-plugin.
func buildSwiftContainerImage(ctx context.Context, dir, product, registryHost, architecture string) error {
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

// ensureBuildxBuilder ensures a buildx builder with the docker-container driver
// exists and returns its name. The docker-container driver is required for
// cross-platform builds that push directly to a registry.
func ensureBuildxBuilder(ctx context.Context) (string, error) {
	const builderName = "wendy"

	// Check if builder already exists.
	cmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", builderName)
	if err := cmd.Run(); err == nil {
		return builderName, nil
	}

	// Create builder with docker-container driver.
	cmd = exec.CommandContext(ctx, "docker", "buildx", "create",
		"--name", builderName,
		"--driver", "docker-container",
		"--driver-opt", "network=host",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("creating buildx builder %q: %s: %w", builderName, string(out), err)
	}

	return builderName, nil
}

// buildAndPushImage builds a Docker image for the specified platform and pushes
// it directly to the given registry using docker buildx. The registry is accessed
// over plain HTTP (insecure).
func buildAndPushImage(ctx context.Context, dir, registryImage, platform string, streamOutput *os.File) error {
	builder, err := ensureBuildxBuilder(ctx)
	if err != nil {
		return err
	}

	args := []string{
		"buildx", "build",
		"--builder", builder,
		"--platform", platform,
		"--output", "type=image,name=" + registryImage + ",push=true,registry.insecure=true",
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
func registryHost(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}
