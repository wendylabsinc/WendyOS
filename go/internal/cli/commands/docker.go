package commands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"strconv"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// neighborExecCommandContext is an overridable wrapper around exec.CommandContext
// used by neighbor-table helpers. Tests can replace this variable to stub
// command execution and outputs.
var neighborExecCommandContext = exec.CommandContext

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
//
// Precedence: compose > Dockerfile > Package.swift > *.xcodeproj > Python markers.
// Returns an error only when multiple .xcodeproj directories are found.
func detectProjectType(dir string) (string, error) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return "compose", nil
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
		return "docker", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "Package.swift")); err == nil {
		return "swift", nil
	}
	xp, err := findXcodeProj(dir)
	if err != nil {
		return "", err
	}
	if xp != "" {
		return "xcode", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		return "python", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "setup.py")); err == nil {
		return "python", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
		return "python", nil
	}
	return "unknown", nil
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

	// Find compose files.
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			options = append(options, BuildOption{
				Label: name + " (Compose)",
				Type:  "compose",
				File:  name,
			})
			break
		}
	}

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

	// Xcode — one entry per .xcodeproj found (independent of Package.swift).
	if err == nil { // entries was read above
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".xcodeproj") {
				options = append(options, BuildOption{
					Label: e.Name() + " (Xcode)",
					Type:  "xcode",
					File:  e.Name(),
				})
			}
		}
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
func injectDebugpy(ctx context.Context, registryAddr, registryImage, platform string, buildArgs map[string]string, streamOutput *os.File, useMTLS bool) error {
	tmpDir, err := os.MkdirTemp("", "wendy-debugpy-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dockerfile := fmt.Sprintf("FROM %s\nUSER root\nRUN pip install debugpy\n", registryImage)
	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("writing debugpy Dockerfile: %w", err)
	}

	return buildAndPushImage(ctx, tmpDir, registryAddr, registryImage, platform, buildArgs, streamOutput, useMTLS)
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

// buildSwiftContainerImage builds a Swift package and pushes the container image
// directly to the device's registry using swift-container-plugin.
// registryAddr is a pre-resolved host:port (e.g. "192.168.1.5:5000" or
// "host.docker.internal:12345" when proxying).
func buildSwiftContainerImage(ctx context.Context, dir, product, registryAddr, architecture string, useMTLS bool, toolchainStdout, toolchainStderr io.Writer) error {
	if err := ensureContainerPlugin(dir); err != nil {
		return err
	}

	sdk, err := swifttoolchain.FindSwiftSDK(ctx, architecture, toolchainStdout, toolchainStderr)
	if err != nil {
		return err
	}

	swiftArgs := []string{
		"package",
		"--swift-sdk=" + sdk,
		"--allow-network-connections=all",
		"build-container-image",
		"--from=swift:" + swifttoolchain.DefaultVersion + "-slim",
		"--product=" + product,
		"--repository=" + registryAddr + "/" + strings.ToLower(product),
		"--architecture=" + architecture,
	}

	// Use insecure HTTP when the connection is not mTLS; the registry only
	// speaks TLS when the device is provisioned and the CLI connected via mTLS.
	if !useMTLS {
		swiftArgs = append(swiftArgs, "--allow-insecure-http=destination")
	}

	cmd := swifttoolchain.SwiftCommandContext(ctx, swiftArgs...)
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
	cmd := swifttoolchain.SwiftCommand("package", "plugin", "--list")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to list Swift package plugins: %w", err)
	}

	if strings.Contains(string(out), "build-container-image") {
		return nil
	}

	fmt.Println("Adding swift-container-plugin dependency...")
	add := swifttoolchain.SwiftCommand("package", "add-dependency",
		"https://github.com/apple/swift-container-plugin", "--from", containerPluginMinVersion)
	add.Dir = dir
	add.Stdout = os.Stdout
	add.Stderr = os.Stderr
	if err := add.Run(); err != nil {
		return fmt.Errorf("failed to add swift-container-plugin dependency: %w", err)
	}

	return nil
}

// dockerRuntimes lists macOS Docker-compatible runtimes in detection order.
// Each entry maps a human-readable name to its .app bundle path.
var dockerRuntimes = []struct{ name, app string }{
	{"OrbStack", "/Applications/OrbStack.app"},
	{"Docker Desktop", "/Applications/Docker.app"},
	{"Rancher Desktop", "/Applications/Rancher Desktop.app"},
}

// ensureDockerDaemon verifies the Docker daemon is running. On macOS, when
// running interactively it prompts the user before launching the installed
// Docker runtime; in non-interactive mode it launches it automatically.
// Waits up to 60 s for the daemon to become ready before returning an error.
func ensureDockerDaemon(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "version").Run() == nil {
		return nil
	}

	if _, err := exec.LookPath("docker"); err != nil {
		if runtime.GOOS == "darwin" && isInteractiveTerminalFn() {
			fmt.Print("Docker is not installed. Install it now with 'brew install --cask docker'? [Y/n] ")
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer != "" && answer != "y" && answer != "yes" {
				return fmt.Errorf("docker is not installed — run: brew install --cask docker")
			}
			fmt.Fprintf(os.Stderr, "[docker] Installing Docker Desktop via Homebrew...\n")
			installCmd := exec.CommandContext(ctx, "brew", "install", "--cask", "docker")
			installCmd.Stdout = os.Stdout
			installCmd.Stderr = os.Stderr
			if err := installCmd.Run(); err != nil {
				return fmt.Errorf("failed to install Docker: %w", err)
			}
			// Fall through to detect and launch the newly installed runtime.
		} else if runtime.GOOS == "darwin" {
			return fmt.Errorf("docker is not installed — run: brew install --cask docker")
		} else {
			return fmt.Errorf("docker is not installed — please install Docker Desktop or OrbStack")
		}
	}

	if runtime.GOOS == "darwin" {
		runtimeName, appPath := detectDockerRuntime()
		if appPath == "" {
			return fmt.Errorf("no supported Docker runtime found — install Docker Desktop or OrbStack and try again")
		}

		if isInteractiveTerminalFn() {
			fmt.Printf("%s is not running. Launch it now? [Y/n] ", runtimeName)
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer != "" && answer != "y" && answer != "yes" {
				return fmt.Errorf("docker daemon is not running — please start %s and try again", runtimeName)
			}
		}

		fmt.Fprintf(os.Stderr, "[docker] Launching %s...\n", runtimeName)
		if err := exec.CommandContext(ctx, "open", "-a", appPath).Run(); err != nil {
			return fmt.Errorf("docker daemon is not running: could not launch %s: %w", runtimeName, err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			if exec.CommandContext(ctx, "docker", "version").Run() == nil {
				fmt.Fprintf(os.Stderr, "[docker] %s is ready\n", runtimeName)
				return nil
			}
		}
		return fmt.Errorf("docker daemon did not become ready within 60 seconds — please start %s manually", runtimeName)
	}

	return fmt.Errorf("docker daemon is not running — please start Docker before using wendy")
}

// detectDockerRuntime returns the name and .app path of the first installed
// Docker-compatible runtime found on macOS, or empty strings if none is found.
func detectDockerRuntime() (name, appPath string) {
	for _, rt := range dockerRuntimes {
		if _, err := os.Stat(rt.app); err == nil {
			return rt.name, rt.app
		}
	}
	return "", ""
}

// ensureBuildxBuilder ensures a buildx builder with the docker-container driver
// exists and returns its name plus the effective registry address to use in
// image references. For IPv6 addresses, a hostname alias is configured inside
// the builder container to avoid brackets that break the TOML parser.
func ensureBuildxBuilder(ctx context.Context, registryAddr string, useMTLS bool) (builderName, effectiveAddr string, err error) {
	if err := ensureDockerDaemon(ctx); err != nil {
		return "", "", err
	}
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
	builderName := os.Getenv("WENDY_BUILDX_BUILDER")
	if builderName == "" {
		builderName = "wendy"
	}

	appliedPath := filepath.Join(configDir, builderName+".applied")

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
		configChanged = true // always inject config into a newly created builder
	}

	// Inject the real config into the builder container and restart only when needed.
	// Also re-inject if the container was destroyed (e.g. after colima restart) or
	// was bootstrapped without config injection (default buildkitd.toml lacks http=true).
	containerName := "buildx_buildkit_" + builderName + "0"

	// Read the config currently applied inside the running container (if any).
	var liveContainerConfig string
	if out, err := exec.CommandContext(ctx, "docker", "exec", containerName,
		"cat", "/etc/buildkit/buildkitd.toml").Output(); err == nil {
		liveContainerConfig = string(out)
	}

	if configChanged || liveContainerConfig != fullConfig {
		if err := updateBuilderConfig(ctx, builderName, fullConfig); err != nil {
			return "", fmt.Errorf("updating builder config: %w", err)
		}
		_ = os.WriteFile(appliedPath, []byte(fullConfig), 0o644)
	} else {
		// Builder exists with correct config — just ensure it's running.
		bootstrapCmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
		if out, err := bootstrapCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
		}
	}

	return builderName, nil
}

// ensureMTLSBuilder ensures the "wendy-mtls" buildx builder exists with mTLS
// client certs for the device registry.
func ensureMTLSBuilder(ctx context.Context, configDir, registryAddr, containerCertDir string) (string, error) {
	base := os.Getenv("WENDY_BUILDX_BUILDER")
	if base == "" {
		base = "wendy"
	}
	builderName := base + "-mtls"

	appliedPath := filepath.Join(configDir, base+"-mtls.applied")

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

	// BuildKit and the agent registry both use Go's TLS stack, which parses
	// every certificate exchanged during the handshake even when verification
	// is disabled or custom. Wendy cloud chains can contain ML-DSA certificates
	// that Go cannot parse, so only present the parseable leaf certificate.
	leafCertPEM, err := certs.LeafCertificatePEM(certInfo.PemCertificate)
	if err != nil {
		return "", fmt.Errorf("extracting client leaf certificate: %w", err)
	}
	if err := os.WriteFile(certPath, []byte(leafCertPEM), 0o644); err != nil {
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
	fmt.Fprintf(os.Stderr, "[buildx] bootstrapping builder %q\n", builderName)
	bootstrapCmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	if out, err := bootstrapCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bootstrapping builder: %s: %w", string(out), err)
	}
	fmt.Fprintf(os.Stderr, "[buildx] bootstrap done\n")

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

	fmt.Fprintf(os.Stderr, "[buildx] copying config into container %q\n", containerName)
	cmd := exec.CommandContext(ctx, "docker", "cp", tmp.Name(), containerName+":"+containerConfigPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp config: %s: %w", string(out), err)
	}

	// Inject a clean Docker config without credsStore so buildkitd (running on
	// Linux inside Colima) does not call docker-credential-osxkeychain, which
	// is a macOS binary that does not exist on Linux and causes "signal: killed"
	// errors when pulling public base images (e.g. python:3.11-slim).
	fmt.Fprintf(os.Stderr, "[buildx] injecting clean docker config into container %q\n", containerName)
	injectCmd := exec.CommandContext(ctx, "docker", "exec", containerName,
		"sh", "-c", `mkdir -p /root/.docker && printf '{"auths":{}}' > /root/.docker/config.json`)
	if out, err := injectCmd.CombinedOutput(); err != nil {
		// Non-fatal: log the error but proceed. The credential helper may still
		// fail for private images, but public images will work without credentials.
		fmt.Fprintf(os.Stderr, "[buildx] warning: could not inject docker config: %s\n", string(out))
	} else {
		fmt.Fprintf(os.Stderr, "[buildx] docker config injected\n")
	}

	fmt.Fprintf(os.Stderr, "[buildx] restarting container %q\n", containerName)
	cmd = exec.CommandContext(ctx, "docker", "restart", containerName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restarting builder: %s: %w", string(out), err)
	}
	fmt.Fprintf(os.Stderr, "[buildx] container restarted, waiting for buildkitd\n")

	bootstrapAfterRestart := exec.CommandContext(ctx, "docker", "buildx", "inspect", "--bootstrap", "--builder", builderName)
	if out, err := bootstrapAfterRestart.CombinedOutput(); err != nil {
		return fmt.Errorf("waiting for builder after restart: %s: %w", string(out), err)
	}
	fmt.Fprintf(os.Stderr, "[buildx] buildkitd ready, sleeping 3s to stabilize proxy\n")

	time.Sleep(3 * time.Second)
	fmt.Fprintf(os.Stderr, "[buildx] builder ready\n")

	return nil
}

// buildAndPushImage builds a Docker image for the specified platform and pushes
// it directly to the given registry using docker buildx. The registry transport
// is conditional: plain HTTP for plaintext devices, and TLS/mTLS for provisioned
// devices when useMTLS is enabled. buildArgs is passed as --build-arg KEY=VALUE flags.
func buildAndPushImage(ctx context.Context, dir, registryAddr, registryImage, platform string, buildArgs map[string]string, streamOutput *os.File, useMTLS bool) error {
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

	// Use a clean Docker config without a credsStore credential helper.
	// On macOS, the default config has "credsStore":"osxkeychain". When
	// docker buildx forwards credentials to buildkitd via the build session,
	// it calls the credential helper on the host. In CI (launchd agent context),
	// the Keychain is inaccessible and the helper is killed → "signal: killed".
	// Public images (e.g. python:3.11-slim) need no credentials; anonymous
	// pull works fine with an empty auths map.
	//
	// We only replace config.json; everything else (cli-plugins, buildx builder
	// instances, contexts) is symlinked from the original Docker config so that
	// buildx and the "wendy" builder remain discoverable.
	origDockerConfig := os.Getenv("DOCKER_CONFIG")
	if origDockerConfig == "" {
		origDockerConfig = filepath.Join(home, ".docker")
	}
	cleanDockerConfigDir := filepath.Join(home, ".cache", "wendy", "docker-config")
	if err := os.MkdirAll(cleanDockerConfigDir, 0o755); err != nil {
		return fmt.Errorf("creating clean docker config directory: %w", err)
	}
	cleanDockerConfigFile := filepath.Join(cleanDockerConfigDir, "config.json")
	if err := os.WriteFile(cleanDockerConfigFile, []byte(`{"auths":{}}`), 0o644); err != nil {
		return fmt.Errorf("writing clean docker config: %w", err)
	}
	// Symlink subdirs that docker/buildx need to find plugins and builder state.
	for _, subdir := range []string{"buildx", "cli-plugins", "contexts"} {
		dst := filepath.Join(cleanDockerConfigDir, subdir)
		if _, err := os.Lstat(dst); err != nil {
			// best-effort: ignore if source doesn't exist or symlink fails
			_ = os.Symlink(filepath.Join(origDockerConfig, subdir), dst)
		}
	}

	args := []string{
		"buildx", "build",
		"--builder", builder,
		"--platform", platform,
		"--cache-from", "type=local,src=" + cacheDir,
		"--cache-to", "type=local,dest=" + cacheDir,
	}
	// Sort keys so the argument order is stable across runs, which keeps
	// build logs reproducible and avoids flakiness in tests that assert args.
	keys := make([]string, 0, len(buildArgs))
	for k := range buildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	args = append(args,
		"--output", "type=image,name="+registryImage+",push=true",
		".",
	)

	fmt.Fprintf(os.Stderr, "[buildx] starting build: docker %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Stdout = streamOutput
	cmd.Stderr = streamOutput
	// Override DOCKER_CONFIG so the buildx client does not call the host
	// credential helper (osxkeychain) when setting up the build session.
	// Filter any existing DOCKER_CONFIG first so our value takes effect.
	baseEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "DOCKER_CONFIG=") {
			baseEnv = append(baseEnv, e)
		}
	}
	cmd.Env = append(baseEnv, "DOCKER_CONFIG="+cleanDockerConfigDir)

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

// resolveRegistry determines how to reach the device registry from Docker buildx.
// Buildkitd runs inside the Docker VM (Colima/Docker Desktop) and cannot reach
// LAN devices directly — only the macOS host can. We always proxy through
// host.docker.internal so buildkitd can push via the host regardless of whether
// the address is link-local or a routable LAN IP.
//
// The returned cleanup function MUST be called when the build is complete to
// stop the proxy and release the port.
func resolveRegistry(ctx context.Context, host string, port int) (registryAddr string, cleanup func(), err error) {
	resolved := resolveRegistryIP(host)

	// On Linux, buildkitd uses host networking (--driver-opt network=host) and
	// can reach LAN devices directly. No proxy needed, and host.docker.internal
	// does not exist on Linux.
	if runtime.GOOS == "linux" {
		addr := resolved
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]"
		}
		return fmt.Sprintf("%s:%d", addr, port), func() {}, nil
	}

	// On macOS, buildkitd runs inside the Colima VM and cannot reach LAN devices
	// directly. Proxy through host.docker.internal so the VM reaches the macOS host,
	// which then forwards to the device.
	//
	// For link-local addresses (USB devices), dial via the original hostname so
	// the host's resolver supplies the zone ID needed for link-local routing.
	// For routable LAN addresses, dial the resolved IP directly.
	var target string
	if isLinkLocalIP(resolved) {
		target = net.JoinHostPort(host, strconv.Itoa(port))
	} else {
		target = net.JoinHostPort(resolved, strconv.Itoa(port))
	}

	proxy, err := startRegistryProxy(ctx, target)
	if err != nil {
		return "", nil, fmt.Errorf("starting registry proxy: %w", err)
	}

	registryAddr = fmt.Sprintf("host.docker.internal:%d", proxy.Port())
	return registryAddr, proxy.Close, nil
}

// resolveRegistryForAgent determines how Docker buildx should reach the
// agent's registry. Cloud connections provide a RegistryDialer that opens a
// fresh broker tunnel per TCP connection; local/LAN connections use the normal
// host-to-device proxy path.
func resolveRegistryForAgent(ctx context.Context, conn *grpcclient.AgentConnection, port int) (registryAddr string, cleanup func(), err error) {
	if conn.RegistryDialer == nil {
		return resolveRegistry(ctx, conn.Host, port)
	}

	proxy, err := startRegistryProxyWithDialer(ctx, func(ctx context.Context) (net.Conn, error) {
		return conn.RegistryDialer(ctx, port)
	})
	if err != nil {
		return "", nil, fmt.Errorf("starting cloud registry proxy: %w", err)
	}

	if runtime.GOOS == "linux" {
		return fmt.Sprintf("127.0.0.1:%d", proxy.Port()), proxy.Close, nil
	}
	return fmt.Sprintf("host.docker.internal:%d", proxy.Port()), proxy.Close, nil
}

// resolveRegistryForSwift is like resolveRegistry but for the Swift container
// plugin, which runs on the host (not inside a Docker VM). Because the host
// can resolve mDNS hostnames directly, we pass the original hostname through
// rather than resolving it to an IP. Only link-local addresses (USB) still
// need the TCP proxy.
func resolveRegistryForSwift(ctx context.Context, host string, port int) (registryAddr string, cleanup func(), err error) {
	resolved := resolveRegistryIP(host)
	if !isLinkLocalIP(resolved) {
		// Use the original hostname (or bare IP) directly — mDNS-resolvable on the host.
		addr := host
		if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
			addr = "[" + addr + "]"
		}
		return fmt.Sprintf("%s:%d", addr, port), func() {}, nil
	}

	// Link-local: same proxy approach as resolveRegistry.
	target := net.JoinHostPort(host, strconv.Itoa(port))
	proxy, err := startRegistryProxy(ctx, target)
	if err != nil {
		return "", nil, fmt.Errorf("starting registry proxy for link-local device: %w", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", proxy.Port()), proxy.Close, nil
}

func resolveRegistryForSwiftAgent(ctx context.Context, conn *grpcclient.AgentConnection, port int) (registryAddr string, cleanup func(), err error) {
	if conn.RegistryDialer == nil {
		return resolveRegistryForSwift(ctx, conn.Host, port)
	}

	proxy, err := startRegistryProxyWithDialer(ctx, func(ctx context.Context) (net.Conn, error) {
		return conn.RegistryDialer(ctx, port)
	})
	if err != nil {
		return "", nil, fmt.Errorf("starting cloud registry proxy for Swift: %w", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", proxy.Port()), proxy.Close, nil
}

// isLinkLocalIP reports whether the given IP string (possibly bracketed) is a
// link-local unicast address (fe80::/10 for IPv6, 169.254.0.0/16 for IPv4).
func isLinkLocalIP(ip string) bool {
	ip = strings.TrimPrefix(ip, "[")
	if idx := strings.Index(ip, "]"); idx >= 0 {
		ip = ip[:idx]
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	return addr.IsLinkLocalUnicast()
}

// registryProxy forwards TCP connections from a local port to a remote device
// registry. This bridges the gap between Docker Desktop's VM (which cannot
// route to link-local addresses) and the host machine (which can).
type registryProxy struct {
	listener net.Listener
	target   string
	dial     func(context.Context) (net.Conn, error)
	cancel   context.CancelFunc
	done     chan struct{}
}

// startRegistryProxy creates a TCP proxy that listens on all interfaces
// (required for Docker Desktop VM connectivity) and forwards connections to
// the target address. The target should use the device's mDNS hostname (not a
// bare link-local IP) so the host's resolver provides the zone ID.
func startRegistryProxy(ctx context.Context, target string) (*registryProxy, error) {
	return startRegistryProxyWithDialer(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}, target)
}

func startRegistryProxyWithDialer(ctx context.Context, dial func(context.Context) (net.Conn, error), target ...string) (*registryProxy, error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}

	proxyCtx, cancel := context.WithCancel(ctx)
	p := &registryProxy{
		listener: ln,
		dial:     dial,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	if len(target) > 0 {
		p.target = target[0]
	}

	go p.serve(proxyCtx)
	return p, nil
}

// Port returns the ephemeral port the proxy is listening on.
func (p *registryProxy) Port() int {
	return p.listener.Addr().(*net.TCPAddr).Port
}

// Close stops the proxy and waits for the serve loop to exit.
func (p *registryProxy) Close() {
	p.cancel()
	p.listener.Close()
	<-p.done
}

func (p *registryProxy) serve(ctx context.Context) {
	defer close(p.done)
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(ctx, conn)
	}
}

func (p *registryProxy) forward(ctx context.Context, client net.Conn) {
	defer client.Close()

	remote, err := p.dial(ctx)
	if err != nil {
		return
	}
	defer remote.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, remote); done <- struct{}{} }()
	<-done
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

	// Scan all addresses before returning — IPv4 may appear after global IPv6
	// in the list (e.g. net.LookupHost on macOS returns AAAA records first).
	var globalIPv6, fallbackLinkLocal string
	for _, a := range addrs {
		addr, parseErr := netip.ParseAddr(a)
		if parseErr != nil {
			continue
		}
		if addr.Is4() {
			return a // IPv4 is always preferred
		}
		if !addr.IsLinkLocalUnicast() {
			if globalIPv6 == "" {
				globalIPv6 = addr.WithZone("").String()
			}
		} else if fallbackLinkLocal == "" {
			fallbackLinkLocal = addr.WithZone("").String()
		}
	}

	if globalIPv6 != "" {
		return globalIPv6
	}

	// DNS returned only link-local IPv6 — this is unroutable from Docker
	// containers (zone IDs are host-specific). As a fallback, try to find
	// the device's IPv4 address by looking up the interface for its IPv6
	// link-local neighbor entry, then selecting an IPv4 neighbor on that
	// same interface. This is common for USB-connected devices where
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
// by its IPv6 link-local address. It looks up the network interface from the
// NDP table, then finds any IPv4 neighbor on that same interface. This works
// because USB point-to-point links typically have only one peer.
//
// Note: MAC correlation is not used because USB RNDIS/ECM adapters often
// assign different MACs to the IPv4 and IPv6 virtual interfaces.
// Returns "" if no IPv4 address can be found.
func findIPv4ViaNeighborTable(ipv6LinkLocal string) string {
	// Use a context that is canceled on interrupt signals (e.g., Ctrl+C),
	// while still enforcing a maximum 2-second timeout for the lookup.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(sigCtx, 2*time.Second)
	defer cancel()

	var candidate string
	switch runtime.GOOS {
	case "darwin":
		candidate = findIPv4NeighborDarwin(ctx, ipv6LinkLocal)
	case "linux":
		candidate = findIPv4NeighborLinux(ctx, ipv6LinkLocal)
	default:
		return ""
	}

	if candidate == "" {
		return ""
	}

	addr, err := netip.ParseAddr(candidate)
	if err != nil || !addr.Is4() {
		return ""
	}

	// Only accept IPv4 link-local (169.254.0.0/16) addresses here to reduce
	// the risk of correlating the IPv6 link-local to the wrong peer on
	// multi-peer interfaces (e.g., Wi-Fi/Ethernet).
	linkLocalPrefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{169, 254, 0, 0}), 16)
	if !linkLocalPrefix.Contains(addr) {
		return ""
	}

	return addr.String()
}

// findIPv4NeighborDarwin looks up the IPv4 address for a device on macOS.
// It finds the interface from the NDP table, then returns the first IPv4
// neighbor on that interface that isn't a local address.
func findIPv4NeighborDarwin(ctx context.Context, ipv6LinkLocal string) string {
	// Step 1: Find the interface from the NDP table.
	// ndp -an output: "fe80::1%en6  aa:bb:cc:dd:ee:ff  en6  23h49m  S  R"
	ndpOut, err := neighborExecCommandContext(ctx, "ndp", "-an").Output()
	if err != nil {
		return ""
	}

	var iface string
	for _, line := range strings.Split(string(ndpOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		addrField := fields[0]
		if idx := strings.Index(addrField, "%"); idx >= 0 {
			addrField = addrField[:idx]
		}
		if addrField != ipv6LinkLocal {
			continue
		}
		iface = fields[2]
		break
	}
	if iface == "" {
		return ""
	}

	// Build a set of local IPv4 addresses on this interface so we can
	// skip them. The ARP table includes "permanent" entries for the
	// host's own addresses which must not be returned as the device IP.
	localAddrs := make(map[string]bool)
	if netIface, ifErr := net.InterfaceByName(iface); ifErr == nil {
		if addrs, addrErr := netIface.Addrs(); addrErr == nil {
			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					localAddrs[ipNet.IP.String()] = true
				}
			}
		}
	}

	// Step 2: Find a non-local IPv4 neighbor on the same interface.
	// arp -an -i en6 output: "? (169.254.189.250) at aa:bb:cc:dd:ee:ff on en6 ..."
	arpOut, err := neighborExecCommandContext(ctx, "arp", "-an", "-i", iface).Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(arpOut), "\n") {
		start := strings.Index(line, "(")
		end := strings.Index(line, ")")
		if start >= 0 && end > start {
			ip := line[start+1 : end]
			if localAddrs[ip] {
				continue
			}
			if parsed, parseErr := netip.ParseAddr(ip); parseErr == nil && parsed.Is4() {
				return ip
			}
		}
	}

	return ""
}

// findIPv4NeighborLinux looks up the IPv4 address for a device on Linux.
// It finds the interface from the IPv6 neighbor table, then returns the
// first non-local IPv4 neighbor on that interface.
func findIPv4NeighborLinux(ctx context.Context, ipv6LinkLocal string) string {
	// Step 1: Find the interface from ip -6 neigh.
	// Output: "fe80::1 dev eth0 lladdr aa:bb:cc:dd:ee:ff STALE"
	// Parse the target IPv6 address once and strip any zone.
	targetAddr, targetErr := netip.ParseAddr(ipv6LinkLocal)
	if targetErr == nil {
		targetAddr = targetAddr.WithZone("")
	}

	neighOut, err := exec.CommandContext(ctx, "ip", "-6", "neigh", "show").Output()
	if err != nil {
		return ""
	}

	var iface string
	for _, line := range strings.Split(string(neighOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		// The first field should be the IPv6 neighbor address, possibly with a zone (e.g., "%eth0").
		addrStr := fields[0]
		if zoneIdx := strings.Index(addrStr, "%"); zoneIdx >= 0 {
			addrStr = addrStr[:zoneIdx]
		}

		parsedAddr, parseErr := netip.ParseAddr(addrStr)
		if parseErr != nil || !parsedAddr.Is6() || targetErr != nil {
			continue
		}
		parsedAddr = parsedAddr.WithZone("")
		if parsedAddr != targetAddr {
			continue
		}

		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				iface = fields[i+1]
				break
			}
		}
		if iface != "" {
			break
		}
	}
	if iface == "" {
		return ""
	}

	// Build a set of local IPv4 addresses on this interface.
	localAddrs := make(map[string]bool)
	if netIface, ifErr := net.InterfaceByName(iface); ifErr == nil {
		if addrs, addrErr := netIface.Addrs(); addrErr == nil {
			for _, a := range addrs {
				if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
					localAddrs[ipNet.IP.String()] = true
				}
			}
		}
	}

	// Step 2: Find a non-local IPv4 neighbor on the same interface.
	// Output: "169.254.189.250 dev usb0 lladdr aa:bb:cc:dd:ee:ff REACHABLE"
	arpOut, err := exec.CommandContext(ctx, "ip", "-4", "neigh", "show", "dev", iface).Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(arpOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			ip := fields[0]
			if localAddrs[ip] {
				continue
			}
			if parsed, parseErr := netip.ParseAddr(ip); parseErr == nil && parsed.Is4() {
				return ip
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
func buildSwiftDockerImage(ctx context.Context, dir, product string, toolchainStdout, toolchainStderr io.Writer) (string, error) {
	arch := runtime.GOARCH
	sdk, err := swifttoolchain.FindSwiftSDK(ctx, arch, toolchainStdout, toolchainStderr)
	if err != nil {
		return "", fmt.Errorf("finding Swift SDK: %w", err)
	}

	cliLogln("Cross-compiling %s for linux/%s...", product, arch)
	buildCmd := swifttoolchain.SwiftCommandContext(ctx,
		"build", "-c", "release", "--swift-sdk="+sdk, "--product", product)
	buildCmd.Dir = dir
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return "", fmt.Errorf("swift build: %w", err)
	}

	// Determine the binary output path.
	showBinCmd := swifttoolchain.SwiftCommandContext(ctx,
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
		swifttoolchain.DefaultVersion)
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
