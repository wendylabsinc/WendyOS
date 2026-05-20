package swifttoolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
)

const (
	DefaultVersion  = "6.3.1"
	WendySDKRelease = "6.3.1-RELEASE"
	wasmSDKChecksum = "bd47baa20771f366d8beed7970afaa30742b2210097afd15f85427226d8f4cf2"
)

var wendySDKChecksums = map[string]string{
	"x86_64":  "8b1e13f35b06fec17cb72ca64257ed51969d1ab1acd8d891251e28bb96ad4e9d",
	"aarch64": "a0155f222bf741e8a4a894c1282941b3a86846fe9d27ac53f94a07d84f897981",
}

var ErrUserCancelled = errors.New("cancelled")

// macOSBrewPaths lists the canonical Homebrew installation locations on macOS,
// checked in order (Apple Silicon first, then Intel). Bypasses $PATH resolution
// to avoid executing an unexpected binary in a compromised environment.
// Security note: these paths are expected to be root-owned on a standard macOS
// installation. A TOCTOU race between stat and exec is theoretically possible
// in unusual environments (e.g. containers with world-writable /opt), but is
// considered an acceptable risk for a developer-facing CLI running as the user.
var macOSBrewPaths = []string{
	"/opt/homebrew/bin/brew", // Apple Silicon (M-series)
	"/usr/local/bin/brew",    // Intel
}

var execCommandContext = exec.CommandContext
var execCommand = exec.Command
var statFile = os.Stat
var currentOS = runtime.GOOS
var confirmFunc = func(question string) (bool, error) {
	return tui.ConfirmDefaultYes(question)
}

func flushWriter(writer io.Writer) {
	if flusher, ok := writer.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func EnsureSwiftVersion(ctx context.Context, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	checkCmd := execCommandContext(ctx, "swiftly", "which", DefaultVersion)
	checkCmd.Stdout = io.Discard
	checkCmd.Stderr = io.Discard
	if err := checkCmd.Run(); err == nil {
		return nil
	} else if errors.Is(err, exec.ErrNotFound) {
		if err := tryBrewInstallSwiftly(ctx, stdout, stderr); err != nil {
			return err
		}
		checkCmd2 := execCommandContext(ctx, "swiftly", "which", DefaultVersion)
		checkCmd2.Stdout = io.Discard
		checkCmd2.Stderr = io.Discard
		if err := checkCmd2.Run(); err == nil {
			return nil
		} else if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("swiftly was installed via Homebrew but is not yet available; " +
				"open a new terminal to reload your PATH")
		}
		// swiftly binary is now available but this version is not installed — fall through to install
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// any other non-nil error (e.g. swiftly which exits non-zero) falls through to swiftly install,
	// matching the original pre-brew-support behaviour

	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := execCommandContext(ctx, "swiftly", "install", DefaultVersion)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	flushWriter(stdout)
	flushWriter(stderr)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("swiftly is required but not installed; see https://swiftlang.github.io/swiftly for installation instructions")
		}
		return fmt.Errorf("installing Swift %s via swiftly: %w", DefaultVersion, err)
	}
	return nil
}

// brewFormula is the tap-qualified Homebrew formula name for swiftly, pinning
// it to the official tap to reduce dependency-confusion risk.
const brewFormula = "swiftlang/swiftly/swiftly"

func tryBrewInstallSwiftly(ctx context.Context, stdout, stderr io.Writer) error {
	if currentOS != "darwin" {
		return fmt.Errorf("swiftly is required but not installed; see https://swiftlang.github.io/swiftly for installation instructions")
	}
	brewPath := ""
	for _, p := range macOSBrewPaths {
		info, err := statFile(p)
		if err != nil {
			continue
		}
		if info.Mode()&0002 != 0 {
			continue // skip world-writable paths — likely not the legitimate brew binary
		}
		brewPath = p
		break
	}
	if brewPath == "" {
		return fmt.Errorf("swiftly is required but not installed; see https://swiftlang.github.io/swiftly for installation instructions")
	}
	confirmed, err := confirmFunc("swiftly is not installed. Install it now via Homebrew? (brew install " + brewFormula + ")")
	if err != nil {
		return fmt.Errorf("swiftly is required but not installed (prompt failed: %w); see https://swiftlang.github.io/swiftly for installation instructions", err)
	}
	if !confirmed {
		return fmt.Errorf("swiftly is required but not installed; run: brew install " + brewFormula)
	}
	fmt.Fprintf(stdout, "Installing swiftly via Homebrew (brew install %s)...\n", brewFormula)
	flushWriter(stdout)
	cmd := execCommandContext(ctx, brewPath, "install", brewFormula)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		flushWriter(stdout)
		flushWriter(stderr)
		return fmt.Errorf("brew install %s: %w", brewFormula, err)
	}
	flushWriter(stdout)
	flushWriter(stderr)
	return nil
}

func SwiftCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	return execCommandContext(ctx, "swiftly", append([]string{"run", "+" + DefaultVersion, "swift"}, args...)...)
}

func SwiftCommand(args ...string) *exec.Cmd {
	return execCommand("swiftly", append([]string{"run", "+" + DefaultVersion, "swift"}, args...)...)
}

func FindSwiftSDK(ctx context.Context, architecture string, stdout, stderr io.Writer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	sdkArch := architecture
	switch sdkArch {
	case "arm64":
		sdkArch = "aarch64"
	case "amd64":
		sdkArch = "x86_64"
	}

	isWasm := sdkArch == "wasm" || sdkArch == "wasm32"

	sdk, err := lookupSwiftSDK(ctx, sdkArch, isWasm)
	if err != nil {
		return "", err
	}
	if sdk != "" {
		return sdk, nil
	}

	if isWasm {
		if err := installWasmSwiftSDK(ctx, stdout, stderr); err != nil {
			return "", err
		}
	} else {
		if err := installWendySwiftSDK(ctx, sdkArch, stdout, stderr); err != nil {
			return "", err
		}
	}

	sdk, err = lookupSwiftSDK(ctx, sdkArch, isWasm)
	if err != nil {
		return "", err
	}
	if sdk == "" {
		return "", fmt.Errorf("Swift SDK installed but not found; run 'swift sdk list' to verify")
	}
	return sdk, nil
}

func lookupSwiftSDK(ctx context.Context, sdkArch string, isWasm bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	out, err := SwiftCommandContext(ctx, "sdk", "list").Output()
	if err != nil {
		return "", fmt.Errorf("running 'swift sdk list': %w (is swiftly installed?)", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")

	if isWasm {
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "wasm") && strings.Contains(line, DefaultVersion) {
				return line, nil
			}
		}
		return "", nil
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "wendyos") && strings.Contains(line, sdkArch) && strings.Contains(line, DefaultVersion) {
			return line, nil
		}
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, sdkArch) && strings.Contains(line, "linux") && strings.Contains(line, DefaultVersion) {
			return line, nil
		}
	}

	return "", nil
}

func installWendySwiftSDK(ctx context.Context, sdkArch string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	sdkName := fmt.Sprintf("%s-RELEASE_wendyos_%s", DefaultVersion, sdkArch)
	url := fmt.Sprintf(
		"https://github.com/wendylabsinc/wendy-swift-tools/releases/download/%s/%s.artifactbundle.zip",
		WendySDKRelease, sdkName,
	)

	fmt.Fprintf(stdout, "Installing WendyOS Swift SDK (%s)...\n", sdkName)

	checksum, ok := wendySDKChecksums[sdkArch]
	if !ok {
		return fmt.Errorf("no checksum available for architecture %s", sdkArch)
	}

	env, cleanup, err := unzipOverwriteEnv()
	if err != nil {
		return fmt.Errorf("setting up unzip wrapper: %w", err)
	}
	defer cleanup()

	cmd := SwiftCommandContext(ctx, "sdk", "install", url, "--checksum", checksum)
	cmd.Env = env
	cmd.Stdout = stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(stderr, &stderrBuf)

	if err := cmd.Run(); err != nil {
		flushWriter(stdout)
		flushWriter(stderr)
		if out := strings.TrimSpace(stderrBuf.String()); out != "" {
			return fmt.Errorf("installing Swift SDK from %s: %w\n%s", url, err, out)
		}
		return fmt.Errorf("installing Swift SDK from %s: %w", url, err)
	}

	flushWriter(stdout)
	flushWriter(stderr)
	fmt.Fprintln(stdout, "Swift SDK installed.")
	flushWriter(stdout)
	return nil
}

func installWasmSwiftSDK(ctx context.Context, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	sdkName := fmt.Sprintf("swift-%s-RELEASE", DefaultVersion)
	url := fmt.Sprintf(
		"https://download.swift.org/swift-%s-release/wasm-sdk/%s/%s_wasm.artifactbundle.tar.gz",
		DefaultVersion, sdkName, sdkName,
	)

	fmt.Fprintf(stdout, "Installing Swift WASM SDK (%s)...\n", sdkName)

	env, cleanup, err := unzipOverwriteEnv()
	if err != nil {
		return fmt.Errorf("setting up unzip wrapper: %w", err)
	}
	defer cleanup()

	cmd := SwiftCommandContext(ctx, "sdk", "install", url, "--checksum", wasmSDKChecksum)
	cmd.Env = env
	cmd.Stdout = stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(stderr, &stderrBuf)

	if err := cmd.Run(); err != nil {
		flushWriter(stdout)
		flushWriter(stderr)
		if out := strings.TrimSpace(stderrBuf.String()); out != "" {
			return fmt.Errorf("installing Swift WASM SDK from %s: %w\n%s", url, err, out)
		}
		return fmt.Errorf("installing Swift WASM SDK from %s: %w", url, err)
	}

	fmt.Fprintln(stdout, "Swift SDK installed.")
	flushWriter(stdout)
	return nil
}

func FindSwiftProduct(dir string) (string, error) {
	return FindSwiftProductWithOptions(dir, "", false)
}

func FindSwiftProductWithOptions(dir, productOverride string, interactive bool) (string, error) {
	var stderr bytes.Buffer
	if productOverride != "" {
		return productOverride, nil
	}

	cmd := SwiftCommand("package", "dump-package")
	cmd.Dir = dir
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = strings.TrimSpace(string(out))
		}
		return "", fmt.Errorf("swift package dump-package failed: %s: %w", errMsg, err)
	}

	var manifest struct {
		Products []struct {
			Name string                     `json:"name"`
			Type map[string]json.RawMessage `json:"type"`
		} `json:"products"`
		Targets []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(out, &manifest); err != nil {
		return "", fmt.Errorf("could not parse Package.swift manifest: %w", err)
	}

	var candidates []string
	for _, product := range manifest.Products {
		if _, ok := product.Type["executable"]; ok {
			candidates = append(candidates, product.Name)
		}
	}

	if len(candidates) == 0 {
		for _, target := range manifest.Targets {
			if target.Type == "executable" {
				candidates = append(candidates, target.Name)
			}
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("Package.swift has no executable products or targets")
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	if !interactive {
		return "", fmt.Errorf("Package.swift has multiple executable products (%s); use --product to select one", strings.Join(candidates, ", "))
	}

	picker := tui.NewPickerWithTitle("Select a Swift product")
	p := tea.NewProgram(picker)

	go func() {
		var items []tui.PickerItem
		for _, name := range candidates {
			n := name
			items = append(items, tui.PickerItem{Name: n, Value: n})
		}
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("product picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return "", ErrUserCancelled
	}

	selected := pm.Selected()
	if selected == nil {
		return "", fmt.Errorf("no product selected")
	}

	return selected.Value.(string), nil
}
