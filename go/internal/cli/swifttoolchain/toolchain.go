package swifttoolchain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
)

const (
	DefaultVersion   = "6.2.3"
	WendySDKRelease  = "0.4.0"
	WasmTargetTriple = "wasm32-unknown-none-wasm"
	wasmSDKChecksum  = "394040ecd5260e68bb02f6c20aeede733b9b90702c2204e178f3e42413edad2a"
)

var wendySDKChecksums = map[string]string{
	"x86_64":  "b5a4d08ad4d4841043727f6671c6aa004da3a2b7f12dc28101d6770c1dc57eb1",
	"aarch64": "ef8fa5a2eda766e3b1df791dc175bbf87f570b9cc6f95ada1fe7643a327e087e",
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
			if strings.Contains(line, "wasm") {
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

	cmd := SwiftCommandContext(ctx, "sdk", "install", url, "--checksum", checksum)
	cmd.Stdout = stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(stderr, &stderrBuf)

	err := cmd.Run()
	flushWriter(stdout)
	flushWriter(stderr)
	if err != nil {
		if out := strings.TrimSpace(stderrBuf.String()); out != "" {
			return fmt.Errorf("installing Swift SDK from %s: %w\n%s", url, err, out)
		}
		return fmt.Errorf("installing Swift SDK from %s: %w", url, err)
	}

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

	cmd := SwiftCommandContext(ctx, "sdk", "install", url, "--checksum", wasmSDKChecksum)
	cmd.Stdout = stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(stderr, &stderrBuf)

	err := cmd.Run()
	flushWriter(stdout)
	flushWriter(stderr)
	if err != nil {
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
