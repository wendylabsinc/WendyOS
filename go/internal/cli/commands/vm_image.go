package commands

import (
	"crypto/sha256"
	"encoding/hex"
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
	if digest != "" && verifyVMImage(cached, digest) == nil {
		return cached, func() {}, nil
	}
	tmp, err := downloadVMImage(&artifact)
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if digest == "" {
		return tmp, cleanup, nil
	}
	if err := verifyVMImage(tmp, digest); err != nil {
		cleanup()
		return "", nil, err
	}
	// Never remove an old cache file before the new download is verified.
	// A concurrent writer has the same digest and therefore identical bytes.
	if err := os.Rename(tmp, cached); err != nil {
		if verifyVMImage(cached, digest) == nil {
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
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("VM image checksum mismatch: got %s, manifest says %s; the build may have changed during download, retry", got, digest)
	}
	return nil
}
