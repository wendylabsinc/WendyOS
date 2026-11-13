package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var downloadVMImage = downloadImage

// Mutable PR labels and URLs are not cache identities. Use the manifest's
// artifact digest, verifying both hits and downloads. Without a checksum,
// download afresh and let the caller remove the temporary file after use.
func resolveVMImage(info *imageInfo) (string, func(), error) {
	dir, err := osCacheDir()
	if err != nil {
		return "", nil, err
	}
	return resolveVMImageIn(dir, info)
}

func resolveVMImageIn(dir string, info *imageInfo) (string, func(), error) {
	return resolveVMImageWith(dir, info, downloadVMImage, verifyVMImage)
}

func resolveVMImageContext(ctx context.Context, info *imageInfo) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	dir, err := osCacheDir()
	if err != nil {
		return "", nil, err
	}
	return resolveVMImageWith(dir, info,
		func(info *imageInfo) (string, error) { return downloadImageContext(ctx, info) },
		func(path, digest string) error { return verifyVMImageContext(ctx, path, digest) })
}

// The MCP path supplies cancellable, noninteractive download and verification
// functions without changing the CLI's process-wide test seams.
func resolveVMImageWith(dir string, info *imageInfo, download func(*imageInfo) (string, error), verify func(string, string) error) (string, func(), error) {
	artifact := *info
	if info.ZstURL != "" {
		artifact.DownloadURL, artifact.Checksum, artifact.ImageSize = info.ZstURL, info.ZstChecksum, 0
	}
	digest := strings.ToLower(strings.TrimSpace(artifact.Checksum))
	if digest != "" {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return "", nil, fmt.Errorf("invalid VM image SHA-256 in manifest")
		}
	}
	cached := filepath.Join(dir, "vm-sha256-"+digest+".image")
	if digest != "" {
		if err := verify(cached, digest); err == nil {
			return cached, func() {}, nil
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", nil, err
		}
	}
	tmp, err := download(&artifact)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if digest == "" {
		return tmp, cleanup, nil
	}
	if err := verify(tmp, digest); err != nil {
		cleanup()
		return "", nil, err
	}
	// Never remove an old cache file before the new download is verified.
	// A concurrent writer has the same digest and therefore identical bytes.
	if err := os.Rename(tmp, cached); err != nil {
		if verify(cached, digest) == nil {
			cleanup()
			return cached, func() {}, nil
		}
		// Windows cannot replace some open cache files. The verified temporary
		// artifact is still usable without deleting a file another reader uses.
		return tmp, cleanup, nil
	}
	return cached, func() {}, nil
}

func verifyVMImage(path, digest string) error {
	return verifyVMImageContext(context.Background(), path, digest)
}

func verifyVMImageContext(ctx context.Context, path, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, simulatorContextReader{ctx: ctx, reader: f}); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("VM image checksum mismatch: got %s, manifest says %s; the build may have changed during download, retry", got, digest)
	}
	return nil
}
