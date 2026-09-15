// Package archive extracts the compressed tarballs wendy downloads. They come
// from a remote manifest, so no entry may write outside the destination.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// ExtractTarGz extracts a .tar.gz into dest.
func ExtractTarGz(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("reading %s as gzip: %w", filepath.Base(tarball), err)
	}
	defer gr.Close() //nolint:errcheck // read-only

	return ExtractTar(gr, dest)
}

// ExtractTarZst extracts a .tar.zst into dest.
func ExtractTarZst(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only

	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	return ExtractTar(zr, dest)
}

// ExtractTar reads a tar stream into dest, which is created fresh. Links are
// skipped rather than recreated: honouring one would be a way out of dest.
func ExtractTar(r io.Reader, dest string) error {
	// Extract into a temp sibling and rename into place, so an interrupted
	// extraction never leaves a half-populated dir that a later open would
	// accept as complete.
	tmp := dest + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := SafeJoin(tmp, hdr.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue // the archive's own "." entry
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			// Mask to owner-writable: keep the exec bit an archive needs
			// (the flashpack ships tools), drop group/world write.
			if err := writeFile(target, tr, os.FileMode(hdr.Mode)&0o755); err != nil {
				return fmt.Errorf("writing %s: %w", hdr.Name, err)
			}
		case tar.TypeSymlink, tar.TypeLink:
			continue
		}
	}

	_ = os.RemoveAll(dest)
	return os.Rename(tmp, dest)
}

// SafeJoin resolves an entry name under root, rejecting anything that would
// escape it. Returns an empty path for an archive's own "." entry.
func SafeJoin(root, name string) (string, error) {
	// IsLocal guarantees that Join stays under root and handles the host's
	// path rules, including Windows drive paths and reserved device names.
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	clean := filepath.Clean(name)
	if clean == "." {
		return "", nil
	}
	return filepath.Join(root, clean), nil
}

func writeFile(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	// Check Close on the success path: a deferred flush can fail here and
	// would otherwise silently truncate the extracted file.
	return out.Close()
}
