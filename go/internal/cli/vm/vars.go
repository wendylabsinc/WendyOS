package vm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ensureVarsStore prepares an x86-64 guest's UEFI variable store before its
// emulator starts. The store must pair with the OVMF code image in use, whose
// template is packaged next to it in 2 MB or 4 MB builds, so it is created as
// a copy of that template and replaced when its size no longer matches, as
// after the firmware package changes. WendyOS boots from the removable-media
// path and keeps its A/B state on the ESP, so the store holds nothing worth
// keeping. An ARM64 store is created zero-filled with the VM and left alone.
//
// Callers hold the VM's run lock, so a running guest's store is never touched.
func ensureVarsStore(spec Spec) error {
	if !spec.Arch.isAMD64() {
		return nil
	}
	if spec.FirmwareVars == "" {
		return errors.New("no UEFI variable store")
	}
	if spec.FirmwareVarsTemplate == "" {
		return errors.New("no UEFI variables template for the x86-64 firmware")
	}
	template, err := os.Stat(spec.FirmwareVarsTemplate)
	if err != nil {
		return fmt.Errorf("reading UEFI variables template %s: %w", spec.FirmwareVarsTemplate, err)
	}
	if !template.Mode().IsRegular() {
		return fmt.Errorf("UEFI variables template %s is not a regular file", spec.FirmwareVarsTemplate)
	}
	if store, err := os.Lstat(spec.FirmwareVars); err == nil {
		if !store.Mode().IsRegular() {
			return fmt.Errorf("UEFI variable store %s is not a regular file", spec.FirmwareVars)
		}
		if store.Size() == template.Size() {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking UEFI variable store: %w", err)
	}
	return copyVarsTemplate(spec.FirmwareVarsTemplate, spec.FirmwareVars)
}

// copyVarsTemplate copies through a temporary file in the VM directory and
// renames it over the store, so an interrupted copy never leaves a store that
// matches the template's size but not its contents.
func copyVarsTemplate(template, store string) (retErr error) {
	src, err := os.Open(template)
	if err != nil {
		return fmt.Errorf("reading UEFI variables template %s: %w", template, err)
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(store), ".efivars-*.tmp")
	if err != nil {
		return fmt.Errorf("creating UEFI variable store: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("creating UEFI variable store: %w", err)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("copying UEFI variables template %s: %w", template, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing UEFI variable store: %w", err)
	}
	if err := os.Rename(tmp.Name(), store); err != nil {
		return fmt.Errorf("installing UEFI variable store: %w", err)
	}
	return nil
}
