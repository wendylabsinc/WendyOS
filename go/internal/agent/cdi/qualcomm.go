package cdi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// qualcommNPUPrefix is where the injected transport appears inside the container. A
// path no image owns, so nothing of the app's is shadowed and the split is explicit.
const qualcommNPUPrefix = "/opt/wendyos/npu/lib"

// The FastRPC transport is locked to the host kernel's driver, so the host must supply
// it. Everything else in the Qualcomm stack is a public SDK download the app pins.
//
// Behind vars so tests can repoint them.
var (
	qualcommTransportGlobs = []string{
		"/usr/lib/lib?dsprpc.so*",
		"/usr/lib/lib?dsp_default_listener.so*",
	}

	// Staged on tmpfs so an OS update is picked up on the next agent start rather
	// than persisting in a container spec.
	qualcommNPUStageDir = "/run/wendyos/npu/lib"

	// The artefacts in the vendor tree an app cannot obtain: the PD shells and the
	// Hexagon C++ runtime every skel links, neither of which ships in the public SDK.
	// Bound per file, so the QNN skels beside them stay the app's to pin.
	qualcommDSPHostLockedGlobs = []string{
		"/usr/share/qcom/conf.d/*.yaml",
		"/usr/share/qcom/conf.d/*.yml",
		"/usr/share/qcom/*/*/*/dsp/*/fastrpc_shell*",
		"/usr/share/qcom/*/*/*/dsp/*/libc++.so*",
		"/usr/share/qcom/*/*/*/dsp/*/libc++abi.so*",
	}

	// The transport nodes the npu entitlement grants; without one this is all inert.
	qualcommDSPDeviceGlob = "/dev/fastrpc-*"
)

// EnsureQualcommNPURuntime stages the kernel-locked FastRPC transport for npu
// containers. Called at agent start, so the staged copy tracks the running OS.
func EnsureQualcommNPURuntime(logger *zap.Logger) {
	if !hasNonSecureFastrpcDevice() {
		return
	}
	if err := os.MkdirAll(qualcommNPUStageDir, 0o755); err != nil {
		logger.Warn("Failed to create Qualcomm NPU runtime dir", zap.Error(err))
		return
	}

	// keep names every source the host still offers, staged or not: a transient failure
	// must not cost the copy already there. staged counts only what was refreshed.
	keep, staged := make(map[string]bool), 0
	for _, glob := range qualcommTransportGlobs {
		matches, err := filepath.Glob(glob)
		if err != nil {
			continue
		}
		for _, src := range matches {
			keep[filepath.Base(src)] = true
			if err := stageRuntimeEntry(src); err != nil {
				logger.Warn("Failed to stage Qualcomm NPU runtime entry",
					zap.String("source", src), zap.Error(err))
				continue
			}
			staged++
		}
	}

	pruneStagedRuntime(keep, logger)
	logger.Info("Staged Qualcomm NPU runtime", zap.Int("libraries", staged))
}

// stageRuntimeEntry copies one library into the stage dir, reading the source before
// replacing the destination so a vanished source cannot delete a good staged copy.
// Symlinks are recreated pointing at the basename, or they dangle inside the bind.
func stageRuntimeEntry(src string) error {
	dst := filepath.Join(qualcommNPUStageDir, filepath.Base(src))

	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return replaceEntry(dst, func(tmp string) error {
			return os.Symlink(filepath.Base(target), tmp)
		})
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return replaceEntry(dst, func(tmp string) error {
		return os.WriteFile(tmp, data, 0o755)
	})
}

// replaceEntry creates the entry under a temporary name and renames it into place, so a
// failure cannot leave the previously staged file deleted.
func replaceEntry(dst string, create func(tmp string) error) error {
	tmp := dst + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := create(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("stage %s: %w", dst, err)
	}
	return nil
}

// pruneStagedRuntime drops entries left by a previous OS version, which the container's
// library path would otherwise still resolve.
func pruneStagedRuntime(keep map[string]bool, logger *zap.Logger) {
	entries, err := os.ReadDir(qualcommNPUStageDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(qualcommNPUStageDir, e.Name())); err != nil {
			logger.Warn("Failed to prune stale Qualcomm NPU runtime entry",
				zap.String("entry", e.Name()), zap.Error(err))
		}
	}
}

// QualcommNPURuntimeResult distinguishes transport provisioning from board-file mounts.
type QualcommNPURuntimeResult struct {
	Mounts           int
	HasDSP           bool
	TransportApplied bool
}

// ApplyQualcommNPURuntime gives an npu-entitled container the driver-locked layer: the
// staged FastRPC transport and the board's DSP firmware tree. The inference framework
// is the app's to bundle.
func ApplyQualcommNPURuntime(spec *oci.Spec) (result QualcommNPURuntimeResult) {
	if !hasNonSecureFastrpcDevice() {
		return result
	}
	result.HasDSP = true

	existing := make(map[string]bool, len(spec.Mounts))
	for _, m := range spec.Mounts {
		existing[m.Destination] = true
	}

	// An empty stage dir means the host has no transport. Report its application
	// separately: board files alone cannot supply the missing transport.
	if dirHasEntries(qualcommNPUStageDir) &&
		addRuntimeBind(spec, existing, qualcommNPUStageDir, qualcommNPUPrefix) {
		result.Mounts++
		result.TransportApplied = true
		if spec.Process != nil {
			spec.Process.Env = prependLibraryPath(spec.Process.Env, qualcommNPUPrefix)
		}
	}

	for _, glob := range qualcommDSPHostLockedGlobs {
		matches, err := filepath.Glob(glob)
		if err != nil {
			continue
		}
		for _, p := range matches {
			if addRuntimeBind(spec, existing, p, p) {
				result.Mounts++
			}
		}
	}

	return result
}

func addRuntimeBind(spec *oci.Spec, existing map[string]bool, source, destination string) bool {
	if existing[destination] {
		return false
	}
	if _, err := os.Stat(source); err != nil {
		return false
	}
	existing[destination] = true
	// noexec is deliberately NOT set: the loader must mmap these executable.
	spec.Mounts = append(spec.Mounts, oci.Mount{
		Destination: destination,
		Source:      source,
		Type:        "bind",
		Options:     []string{"rbind", "ro", "nosuid", "nodev"},
	})
	return true
}

// prependLibraryPath puts dir ahead of the app's own search path, so the kernel-locked
// transport wins over a bundled copy that may not match this kernel. OCI env is
// last-wins, so the effective entry is the last one and the result is appended.
func prependLibraryPath(env []string, dir string) []string {
	const key = "LD_LIBRARY_PATH="
	existing := ""
	kept := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, key) {
			existing = e[len(key):]
			continue
		}
		kept = append(kept, e)
	}
	if existing != "" {
		return append(kept, key+dir+":"+existing)
	}
	return append(kept, key+dir)
}

// dirHasEntries reports whether path is a directory with at least one entry.
func dirHasEntries(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

// hasNonSecureFastrpcDevice reports whether the host exposes a FastRPC node the
// entitlement can grant. The -secure nodes are never granted, so a board carrying only
// those counts as having no DSP here.
func hasNonSecureFastrpcDevice() bool {
	matches, err := filepath.Glob(qualcommDSPDeviceGlob)
	if err != nil {
		return false
	}
	for _, m := range matches {
		if oci.IsGrantableFastrpcNode(m) {
			return true
		}
	}
	return false
}
