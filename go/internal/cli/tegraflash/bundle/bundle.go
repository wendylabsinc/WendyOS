// bundle extracts files from a tegraflash tarball produced by the WendyOS CI.
// The applet binaries inside the tarball are NVIDIA proprietary — this package
// never embeds them; it extracts them at runtime from the user's own bundle,
// which they obtained under NVIDIA's Jetson Software License Agreement.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Bundle provides access to files inside a tegraflash tarball.
type Bundle struct {
	path string
}

// Open opens the bundle at the given path (must be a .tar.gz or .tar file).
func Open(path string) (*Bundle, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("bundle not found at %s: %w", path, err)
	}
	return &Bundle{path: path}, nil
}

// Close is a no-op; bundles are read on demand.
func (b *Bundle) Close() error { return nil }

// Applet returns the contents of the recovery applet binary.
// Tries T264 (Thor) first, then T234 (Orin) as fallback.
func (b *Bundle) Applet() ([]byte, error) {
	for _, name := range []string{"applet_t264.bin", "applet_t234.bin"} {
		data, err := b.extract(name)
		if err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("applet binary not found in bundle (tried applet_t264.bin, applet_t234.bin)")
}

// ExtractFile extracts a file by basename from the bundle.
func (b *Bundle) ExtractFile(name string) ([]byte, error) {
	return b.extract(strings.TrimSpace(name))
}

// FindXML returns the contents and filename of the partition layout XML.
// fullEMMC selects the full eMMC layout; false selects recovery/QSPI boot only.
func (b *Bundle) FindXML(fullEMMC bool) ([]byte, string, error) {
	var candidates []string
	if fullEMMC {
		// Full eMMC flash: write all partitions.
		candidates = []string{
			"flash.xml.in",
			"flash_t234_qspi_sdmmc.xml",
			"flash_l4t_t234_sdmmc.xml",
			"flash_t234.xml",
		}
	} else {
		// Recovery / QSPI-only boot firmware update.
		candidates = []string{
			"rcmboot-flash.xml.in",
			"flash_t234_qspi.xml",
			"flash_l4t_t234_nvme.xml",
		}
	}
	for _, name := range candidates {
		data, err := b.extract(name)
		if err == nil {
			return data, name, nil
		}
	}

	// Fall back: find any .xml or .xml.in file in the bundle that looks like a partition layout.
	files, err := b.ListFiles()
	if err != nil {
		return nil, "", fmt.Errorf("no partition layout XML found in bundle")
	}
	for _, f := range files {
		base := filepath.Base(f)
		if (strings.HasSuffix(base, ".xml") || strings.HasSuffix(base, ".xml.in")) && strings.Contains(base, "flash") {
			data, err := b.extract(base)
			if err == nil {
				return data, base, nil
			}
		}
	}

	return nil, "", fmt.Errorf("no partition layout XML found in bundle — expected flash_t234_*.xml")
}

// openTar opens the bundle file and returns a tar.Reader. It detects gzip by
// reading the first two magic bytes rather than relying on the file extension,
// since CI sometimes produces plain-tar bundles with a .tar.gz suffix.
// The caller must close f when done.
func (b *Bundle) openTar() (*tar.Reader, *os.File, func(), error) {
	f, err := os.Open(b.path)
	if err != nil {
		return nil, nil, nil, err
	}

	magic := make([]byte, 2)
	if _, err := io.ReadFull(f, magic); err != nil {
		f.Close()
		return nil, nil, nil, fmt.Errorf("reading archive header: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, nil, fmt.Errorf("seeking archive: %w", err)
	}

	isGzip := magic[0] == 0x1f && magic[1] == 0x8b
	if isGzip {
		gr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, nil, fmt.Errorf("opening gzip stream: %w", err)
		}
		return tar.NewReader(gr), f, func() { gr.Close(); f.Close() }, nil
	}
	return tar.NewReader(f), f, func() { f.Close() }, nil
}

// extract reads the first file matching name (basename only) from the archive.
func (b *Bundle) extract(name string) ([]byte, error) {
	tr, _, cleanup, err := b.openTar()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if filepath.Base(hdr.Name) == name {
			return io.ReadAll(tr)
		}
	}

	return nil, fmt.Errorf("%s not found in bundle %s", name, filepath.Base(b.path))
}

// ListFiles returns the names of all files in the bundle (for debugging).
func (b *Bundle) ListFiles() ([]string, error) {
	tr, _, cleanup, err := b.openTar()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
	return names, nil
}
