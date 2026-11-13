// Package arduinotoolchain compiles Arduino sketches for a board's
// microcontroller with a pinned arduino-cli that Wendy downloads and keeps in
// its own cache, apart from any Arduino IDE setup on the machine.
package arduinotoolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// CLIVersion is the arduino-cli release Wendy compiles sketches with.
const CLIVersion = "1.5.1"

const (
	downloadBase   = "https://github.com/arduino/arduino-cli/releases/download/v" + CLIVersion + "/"
	maxArchiveSize = 64 << 20
)

type archive struct{ File, SHA256, Binary string }

// archives pins each host's release archive by its published SHA-256.
var archives = map[string]archive{
	"linux/amd64":   {"arduino-cli_1.5.1_Linux_64bit.tar.gz", "28a8e119c498a25607821c36cb2dc49e8463941b261a0d99091baa7bc692dd2b", "arduino-cli"},
	"linux/arm64":   {"arduino-cli_1.5.1_Linux_ARM64.tar.gz", "1e69e077479f300614d4551334e0a33f08ee40b04315d83b8e7e0e94f0d0ee62", "arduino-cli"},
	"darwin/amd64":  {"arduino-cli_1.5.1_macOS_64bit.tar.gz", "c982e940027996bea9901050e95fae99c59c1dcfee54beedecaf28141e7bf2e7", "arduino-cli"},
	"darwin/arm64":  {"arduino-cli_1.5.1_macOS_ARM64.tar.gz", "cb952e8c1621c95ef5f1d17831c945e3d0ec5973f89c557a7ec8feb9c4f7d4c9", "arduino-cli"},
	"windows/amd64": {"arduino-cli_1.5.1_Windows_64bit.zip", "fabe42e0eb04d00e776a66178299ff95a46c623dbc260f997e58fd514853dd40", "arduino-cli.exe"},
}

var (
	cacheDir           = config.CacheDir
	httpClient         = http.DefaultClient
	execCommandContext = exec.CommandContext
	hostPlatform       = func() string { return runtime.GOOS + "/" + runtime.GOARCH }
)

// EnsureCLI returns the pinned arduino-cli, downloading and verifying it on
// first use, and writes the config that keeps its data in Wendy's cache.
func EnsureCLI(ctx context.Context, stderr io.Writer) (string, error) {
	host := hostPlatform()
	a, ok := archives[host]
	if !ok {
		return "", fmt.Errorf("arduino-cli %s has no build for %s", CLIVersion, host)
	}
	root, err := cacheDir()
	if err != nil {
		return "", err
	}
	if err := writeConfig(root); err != nil {
		return "", err
	}
	dir := filepath.Join(root, "toolchains", "arduino-cli", CLIVersion)
	bin := filepath.Join(dir, a.Binary)
	if fi, err := os.Stat(bin); err == nil && fi.Mode().IsRegular() {
		return bin, nil
	}

	fmt.Fprintf(stderr, "Downloading arduino-cli %s...\n", CLIVersion)
	data, err := download(ctx, downloadBase+a.File)
	if err != nil {
		return "", err
	}
	if got := sha256Hex(data); got != a.SHA256 {
		return "", fmt.Errorf("arduino-cli download SHA-256 mismatch: got %s, want %s", got, a.SHA256)
	}
	binary, err := extract(data, a)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".arduino-cli-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), bin); err != nil {
		return "", err
	}
	return bin, nil
}

func configPath(root string) string {
	return filepath.Join(root, "arduino", "arduino-cli.yaml")
}

func writeConfig(root string) error {
	base := filepath.Join(root, "arduino")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	cfg := fmt.Sprintf("directories:\n  data: %q\n  downloads: %q\n  user: %q\n",
		filepath.Join(base, "data"), filepath.Join(base, "downloads"), filepath.Join(base, "user"))
	return os.WriteFile(configPath(root), []byte(cfg), 0o644)
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading arduino-cli: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading arduino-cli: HTTP %d from %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveSize+1))
	if err != nil {
		return nil, fmt.Errorf("downloading arduino-cli: %w", err)
	}
	if len(data) > maxArchiveSize {
		return nil, fmt.Errorf("arduino-cli download exceeds %d MiB", maxArchiveSize>>20)
	}
	return data, nil
}

func extract(data []byte, a archive) ([]byte, error) {
	if strings.HasSuffix(a.File, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("reading arduino-cli archive: %w", err)
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == a.Binary {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxArchiveSize))
			}
		}
		return nil, fmt.Errorf("arduino-cli archive has no %s", a.Binary)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading arduino-cli archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("arduino-cli archive has no %s", a.Binary)
		}
		if err != nil {
			return nil, fmt.Errorf("reading arduino-cli archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == a.Binary {
			return io.ReadAll(io.LimitReader(tr, maxArchiveSize))
		}
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
