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
	"path/filepath"
	"sort"
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
	"x86_64":  "982bb4f1a3632e628d63cf5f7478e7ec12264dd13755b709f6dd40853b56ab92",
	"aarch64": "506a6f002f3c434af79fb1396c3e13adbd18d8e2b294c7627b93d6fc51f29a34",
}

var ErrUserCancelled = errors.New("cancelled")

var execCommandContext = exec.CommandContext
var execCommand = exec.Command

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
	} else {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("swiftly is required but not installed; see https://swiftlang.github.io/swiftly for installation instructions")
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}

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

	expectedTriple, err := expectedLinuxSDKTriple(sdkArch)
	if err != nil {
		return "", err
	}

	var validationErr error
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "wendyos") && strings.Contains(line, sdkArch) && strings.Contains(line, DefaultVersion) {
			if err := validateInstalledSwiftSDKVariant(line, expectedTriple); err != nil {
				validationErr = err
				continue
			}
			return line, nil
		}
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, sdkArch) && strings.Contains(line, "linux") && strings.Contains(line, DefaultVersion) {
			return line, nil
		}
	}

	if validationErr != nil {
		return "", validationErr
	}
	return "", nil
}

// unzipOverwriteEnv returns a modified env with a unzip wrapper prepended to
// PATH. The wrapper passes -o (overwrite without prompting) to the real unzip
// binary, which prevents interactive prompts when the zip has duplicate entries.
// Call the returned cleanup func when done.
func unzipOverwriteEnv() (env []string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "wendy-unzip-*")
	if err != nil {
		return nil, func() {}, err
	}
	script := "#!/bin/sh\nexec /usr/bin/unzip -o \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "unzip"), []byte(script), 0755); err != nil {
		os.RemoveAll(dir)
		return nil, func() {}, err
	}
	env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	return env, func() { os.RemoveAll(dir) }, nil
}

func expectedLinuxSDKTriple(sdkArch string) (string, error) {
	switch sdkArch {
	case "aarch64":
		return "aarch64-unknown-linux-gnu", nil
	case "x86_64":
		return "x86_64-unknown-linux-gnu", nil
	default:
		return "", fmt.Errorf("unsupported linux SDK architecture %q", sdkArch)
	}
}

type swiftSDKBundleInfo struct {
	Artifacts map[string]struct {
		Type     string `json:"type"`
		Variants []struct {
			Path string `json:"path"`
		} `json:"variants"`
	} `json:"artifacts"`
}

func validateInstalledSwiftSDKVariant(sdkName, expectedTriple string) error {
	if !strings.Contains(sdkName, "wendyos") {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	roots := []string{
		filepath.Join(home, "Library", "org.swift.swiftpm", "swift-sdks"),
		filepath.Join(home, ".swiftpm", "swift-sdks"),
	}

	var infoPath string
	for _, root := range roots {
		candidate := filepath.Join(root, sdkName+".artifactbundle", "info.json")
		if _, err := os.Stat(candidate); err == nil {
			infoPath = candidate
			break
		}
	}
	if infoPath == "" {
		return nil
	}

	data, err := os.ReadFile(infoPath)
	if err != nil {
		return fmt.Errorf("reading Swift SDK metadata for %s: %w", sdkName, err)
	}

	var info swiftSDKBundleInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("parsing Swift SDK metadata for %s: %w", sdkName, err)
	}

	artifact, ok := info.Artifacts[sdkName]
	if !ok {
		return nil
	}

	variants := make([]string, 0, len(artifact.Variants))
	for _, variant := range artifact.Variants {
		variants = append(variants, filepath.Base(variant.Path))
		if filepath.Base(variant.Path) == expectedTriple {
			return nil
		}
	}
	if len(variants) == 0 {
		return fmt.Errorf("Swift SDK %q does not declare any target variants in %s", sdkName, infoPath)
	}
	sort.Strings(variants)
	return fmt.Errorf("Swift SDK %q provides %s, not %s; reinstall or fix the SDK artifact", sdkName, strings.Join(variants, ", "), expectedTriple)
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
