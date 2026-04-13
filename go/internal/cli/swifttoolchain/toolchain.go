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
	"strings"
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

var execCommandContext = exec.CommandContext
var execCommand = exec.Command

func EnsureSwiftVersion(ctx context.Context, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
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
	if flusher, ok := stdout.(interface{ Flush() }); ok {
		flusher.Flush()
	}
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

func FindSwiftSDK(ctx context.Context, architecture string) (string, error) {
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
		if err := installWasmSwiftSDK(ctx); err != nil {
			return "", err
		}
	} else {
		if err := installWendySwiftSDK(ctx, sdkArch); err != nil {
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

func installWendySwiftSDK(ctx context.Context, sdkArch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	sdkName := fmt.Sprintf("%s-RELEASE_wendyos_%s", DefaultVersion, sdkArch)
	url := fmt.Sprintf(
		"https://github.com/wendylabsinc/wendy-swift-tools/releases/download/%s/%s.artifactbundle.zip",
		WendySDKRelease, sdkName,
	)

	fmt.Printf("Installing WendyOS Swift SDK (%s)...\n", sdkName)

	checksum, ok := wendySDKChecksums[sdkArch]
	if !ok {
		return fmt.Errorf("no checksum available for architecture %s", sdkArch)
	}

	cmd := SwiftCommandContext(ctx, "sdk", "install", url, "--checksum", checksum)
	cmd.Stdout = os.Stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	if err := cmd.Run(); err != nil {
		if out := strings.TrimSpace(stderrBuf.String()); out != "" {
			return fmt.Errorf("installing Swift SDK from %s: %w\n%s", url, err, out)
		}
		return fmt.Errorf("installing Swift SDK from %s: %w", url, err)
	}

	fmt.Println("Swift SDK installed.")
	return nil
}

func installWasmSwiftSDK(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	sdkName := fmt.Sprintf("swift-%s-RELEASE", DefaultVersion)
	url := fmt.Sprintf(
		"https://download.swift.org/swift-%s-release/wasm-sdk/%s/%s_wasm.artifactbundle.tar.gz",
		DefaultVersion, sdkName, sdkName,
	)

	fmt.Printf("Installing Swift WASM SDK (%s)...\n", sdkName)

	cmd := SwiftCommandContext(ctx, "sdk", "install", url, "--checksum", wasmSDKChecksum)
	cmd.Stdout = os.Stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	if err := cmd.Run(); err != nil {
		if out := strings.TrimSpace(stderrBuf.String()); out != "" {
			return fmt.Errorf("installing Swift WASM SDK from %s: %w\n%s", url, err, out)
		}
		return fmt.Errorf("installing Swift WASM SDK from %s: %w", url, err)
	}

	fmt.Println("Swift SDK installed.")
	return nil
}

func FindSwiftProduct(dir string) (string, error) {
	cmd := SwiftCommand("package", "dump-package")
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
		for _, product := range manifest.Products {
			productNames = append(productNames, product.Name)
		}
		return "", fmt.Errorf("Package.swift declares multiple products (%s); wendy run requires a single executable product", strings.Join(productNames, ", "))
	}

	var execTargets []string
	for _, target := range manifest.Targets {
		if target.Type == "executable" {
			execTargets = append(execTargets, target.Name)
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
