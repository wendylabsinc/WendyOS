package services

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"go.uber.org/zap"
)

const (
	jetsonEFIVarsDir = "/sys/firmware/efi/efivars"
	jetsonEFIGUID    = "781e084c-a330-417c-b678-38e696380cb9"
	jetsonAPPBPath   = "/dev/disk/by-partlabel/APP_b"
	rootfsRedundancy = "RootfsRedundancyLevel"
	rootfsRetryMax   = "RootfsRetryCountMax"
	efiBootRuntime   = uint32(0x06)
	efiNonVolatile   = uint32(0x01)
)

// prepareJetsonRootfsAB repairs writable, persistent Jetson A/B settings before
// either UpdateOS API starts an install. Some firmware supplies locked volatile
// defaults instead: these cannot be repaired persistently by a runtime write.
// Do not treat a successful write of a volatile value as readiness for reboot.
func prepareJetsonRootfsAB(ctx context.Context, logger *zap.Logger) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	p := jetsonRootfsPreparation{
		efivarsDir: jetsonEFIVarsDir,
		appBPath:   jetsonAPPBPath,
		lookPath:   exec.LookPath,
		writeVar:   writeJetsonEFIVariable,
	}
	changed, err := p.prepare(ctx)
	if err != nil {
		return fmt.Errorf("cannot prepare Jetson rootfs A/B redundancy: %w", err)
	}
	if changed {
		logger.Info("Prepared Jetson rootfs A/B redundancy; verified EFI variable read-back")
	}
	return nil
}

// Paths and the writer are per-instance so tests never access host firmware.
type jetsonRootfsPreparation struct {
	efivarsDir string
	appBPath   string
	lookPath   func(string) (string, error)
	writeVar   func(string, []byte) error
}

type jetsonEFIVariable struct {
	attributes uint32
	value      uint32
}

func (p jetsonRootfsPreparation) variablePath(name string) string {
	return filepath.Join(p.efivarsDir, name+"-"+jetsonEFIGUID)
}

func (p jetsonRootfsPreparation) readVariable(name string) (jetsonEFIVariable, error) {
	raw, err := os.ReadFile(p.variablePath(name))
	if errors.Is(err, os.ErrNotExist) {
		// A missing variable can be created only as persistent NV|BS|RT.
		return jetsonEFIVariable{attributes: efiBootRuntime | efiNonVolatile}, nil
	}
	if err != nil {
		return jetsonEFIVariable{}, fmt.Errorf("read %s: %w", name, err)
	}
	if len(raw) != 8 {
		return jetsonEFIVariable{}, fmt.Errorf("%s has %d bytes; expected an 8-byte EFI UINT32 variable", name, len(raw))
	}
	v := jetsonEFIVariable{
		attributes: binary.LittleEndian.Uint32(raw[:4]),
		value:      binary.LittleEndian.Uint32(raw[4:]),
	}
	if v.attributes != efiBootRuntime && v.attributes != efiBootRuntime|efiNonVolatile {
		return jetsonEFIVariable{}, fmt.Errorf("%s has unsupported EFI attributes %#x", name, v.attributes)
	}
	return v, nil
}

func (p jetsonRootfsPreparation) prepare(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := p.lookPath("nvbootctrl"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return false, nil // Other boards do not use NVIDIA's EFI variables.
		}
		return false, fmt.Errorf("find nvbootctrl: %w", err)
	}
	if _, err := os.Stat(p.appBPath); err != nil {
		return false, fmt.Errorf("cannot access alternate rootfs partition APP_b; an A/B partition layout is required: %w", err)
	}
	// Check the mount before reading variables: an inaccessible/missing efivarfs
	// must not look like two missing variables that the agent can create.
	info, err := os.Stat(p.efivarsDir)
	if err != nil {
		return false, fmt.Errorf("access EFI variables: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("EFI variable path %s is not a directory", p.efivarsDir)
	}
	level, err := p.readVariable(rootfsRedundancy)
	if err != nil {
		return false, err
	}
	if level.value > 1 {
		return false, fmt.Errorf("%s has unsupported level %d", rootfsRedundancy, level.value)
	}
	retries, err := p.readVariable(rootfsRetryMax)
	if err != nil {
		return false, err
	}
	// NVIDIA stores each slot's remaining retries in a two-bit scratch field.
	// A larger value can truncate to zero and leave the slot with no attempts.
	if retries.value > 3 {
		return false, fmt.Errorf("%s has unsupported retry count %d; expected 0 through 3", rootfsRetryMax, retries.value)
	}
	// Preserve firmware-provided settings, including valid volatile defaults.
	// Only missing/disabled settings need a persistent repair. Validate both
	// before writing either, so a protected level cannot cause a partial repair.
	for _, setting := range []struct {
		name string
		v    jetsonEFIVariable
	}{{rootfsRedundancy, level}, {rootfsRetryMax, retries}} {
		if setting.v.value == 0 && setting.v.attributes&efiNonVolatile == 0 {
			return false, fmt.Errorf("%s is disabled and firmware-managed (volatile EFI attributes %#x); a runtime write would not survive reboot. Enable rootfs A/B in the firmware configuration or reflash with an A/B-enabled image", setting.name, setting.v.attributes)
		}
	}
	changed := false
	// Prepare the retry budget before enabling redundancy. Do not rewrite a
	// valid retry count, especially the locked volatile default on NVIDIA UEFI.
	if retries.value == 0 {
		if err := p.writeAndVerify(ctx, rootfsRetryMax, retries.attributes, 3); err != nil {
			return false, err
		}
		changed = true
	}
	if level.value == 0 {
		if err := p.writeAndVerify(ctx, rootfsRedundancy, level.attributes, 1); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func (p jetsonRootfsPreparation) writeAndVerify(ctx context.Context, name string, attributes, value uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint32(raw[:4], attributes)
	binary.LittleEndian.PutUint32(raw[4:], value)
	if err := p.writeVar(p.variablePath(name), raw); err != nil {
		return fmt.Errorf("write %s: %w; firmware may lock this setting. Enable rootfs A/B in the firmware configuration or reflash with an A/B-enabled image", name, err)
	}
	v, err := p.readVariable(name)
	if err != nil {
		return fmt.Errorf("verify %s: %w", name, err)
	}
	if v.attributes != attributes || v.value != value {
		return fmt.Errorf("verify %s: read-back attributes=%#x value=%d, expected attributes=%#x value=%d", name, v.attributes, v.value, attributes, value)
	}
	return nil
}
