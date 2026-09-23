package services

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/squashfs"
)

const (
	// Relative on purpose: go-diskfs validates paths with io/fs, which rejects a
	// leading slash as "invalid argument".
	extensionReleaseDir = "usr/lib/extension-release.d/extension-release."
	// Written by pack-sysext.sh; the same field the on-device apply script reads.
	imageKernelField = "WENDYOS_KERNEL"
	// A self-describing add-on bakes its autoload list here, at the path it will
	// occupy once merged.
	imageModulesDir = "usr/lib/modules-load.d/"
)

// parseKeyValues reads an os-release style KEY="value" file. Malformed lines are
// skipped rather than failing the parse, matching systemd's own leniency.
func parseKeyValues(r io.Reader) map[string]string {
	vals := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		k, v, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		vals[k] = strings.Trim(v, `"`)
	}
	return vals
}

// readExtensionRelease returns the extension-release fields the add-on image
// declares for name. The file is named after the add-on, so a wrong name is
// indistinguishable from a wrong image - systemd refuses to merge either.
func readExtensionRelease(rawPath, name string) (map[string]string, error) {
	b, err := file.OpenFromPath(rawPath, true)
	if err != nil {
		return nil, fmt.Errorf("opening add-on image: %w", err)
	}
	defer b.Close()

	info, err := b.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading add-on image: %w", err)
	}
	fs, err := squashfs.Read(b, info.Size(), 0, 0)
	if err != nil {
		return nil, fmt.Errorf("add-on image is not a readable squashfs: %w", err)
	}
	content, err := fs.ReadFile(extensionReleaseDir + name)
	if err != nil {
		return nil, fmt.Errorf("add-on image declares no extension-release for %q; it was built under a different name", name)
	}
	return parseKeyValues(strings.NewReader(string(content))), nil
}

// verifyImageKernel rejects an add-on whose own image says it targets a
// different kernel. The image is the authority: a caller may omit the version
// or, for a registry install, pass one the manifest got wrong.
func verifyImageKernel(rawPath, name, declared, running string) error {
	fields, err := readExtensionRelease(rawPath, name)
	if err != nil {
		return err
	}
	want := fields[imageKernelField]
	if want == "" {
		// An add-on carrying no kernel modules (udev rules, firmware) is valid and
		// pins no kernel. The apply script treats an absent field the same way.
		return nil
	}
	if want != running {
		return fmt.Errorf("driver %q was built for kernel %s but this device runs %s", name, want, running)
	}
	if declared != "" && declared != want {
		return fmt.Errorf("driver %q image declares kernel %s but it was published as %s", name, want, declared)
	}
	return nil
}

// imageKernel reports the kernel an add-on was built for and whether its image
// could be read at all. An unreadable image and one that pins no kernel both
// yield "", but only the first means the add-on is broken, so callers must not
// treat them alike: systemd-sysext will not merge an image it cannot parse.
func imageKernel(rawPath, name string) (kernel string, readable bool) {
	fields, err := readExtensionRelease(rawPath, name)
	if err != nil {
		return "", false
	}
	return fields[imageKernelField], true
}

// imageModules returns the modules an add-on autoloads, read from the image
// itself. The on-disk copy only exists once the add-on is merged, so this is the
// only way to know the list before installing it. Absent is not an error: an
// add-on may ship none, or carry a /data override instead.
func imageModules(rawPath, name string) []string {
	b, err := file.OpenFromPath(rawPath, true)
	if err != nil {
		return nil
	}
	defer b.Close()
	info, err := b.Stat()
	if err != nil {
		return nil
	}
	fs, err := squashfs.Read(b, info.Size(), 0, 0)
	if err != nil {
		return nil
	}
	content, err := fs.ReadFile("usr/lib/wendyos-driver-payloads/" + name + "/modules-load.conf")
	if err != nil {
		content, err = fs.ReadFile(imageModulesDir + name + ".conf")
	}
	if err != nil {
		return nil
	}
	return parseModulesConf(strings.NewReader(string(content)))
}
