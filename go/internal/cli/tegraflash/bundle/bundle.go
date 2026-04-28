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

// Applet returns the contents of the T234 recovery applet binary.
// The file is named applet_t234.bin inside the tarball.
func (b *Bundle) Applet() ([]byte, error) {
	return b.extract("applet_t234.bin")
}

// ExtractFile extracts a file by basename from the bundle.
func (b *Bundle) ExtractFile(name string) ([]byte, error) {
	return b.extract(strings.TrimSpace(name))
}

// FindXML returns the contents and filename of the partition layout XML.
// It tries common T234 layout XML names in order.
func (b *Bundle) FindXML() ([]byte, string, error) {
	// Common XML filenames in L4T tegraflash bundles (T234 variants).
	// flash_t234_qspi.xml is the QSPI-only layout used by the NVMe WendyOS bundle.
	// flash.xml.in is used by the eMMC bundle (NVIDIA's template convention).
	candidates := []string{
		"flash.xml.in",
		"flash_t234_qspi.xml",
		"flash_t234_qspi_sdmmc.xml",
		"flash_l4t_t234_nvme.xml",
		"flash_l4t_t234_sdmmc.xml",
		"flash_t234.xml",
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

// extract reads the first file matching name (basename only) from the archive.
func (b *Bundle) extract(name string) ([]byte, error) {
	f, err := os.Open(b.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var tr *tar.Reader
	if strings.HasSuffix(b.path, ".gz") || strings.HasSuffix(b.path, ".tgz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("opening gzip stream: %w", err)
		}
		defer gr.Close()
		tr = tar.NewReader(gr)
	} else {
		tr = tar.NewReader(f)
	}

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
	f, err := os.Open(b.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var tr *tar.Reader
	if strings.HasSuffix(b.path, ".gz") || strings.HasSuffix(b.path, ".tgz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		tr = tar.NewReader(gr)
	} else {
		tr = tar.NewReader(f)
	}

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
