package cdi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// useQualcommRuntimeDir stages a fake host in a tempdir and points every path at it, so
// the suite never reads the real /usr/share/qcom -- which exists on the boards this
// feature targets and would otherwise decide the result.
func useQualcommRuntimeDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", n, err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}

	origTransport, origStage := qualcommTransportGlobs, qualcommNPUStageDir
	origLocked, origDevice := qualcommDSPHostLockedGlobs, qualcommDSPDeviceGlob

	qualcommTransportGlobs = []string{filepath.Join(dir, "lib?dsprpc.so*")}
	qualcommNPUStageDir = filepath.Join(dir, "stage")
	// Derived from the production patterns rather than restated, so a change there is
	// exercised here instead of being masked by a stale stub.
	rebased := make([]string, 0, len(origLocked))
	for _, g := range origLocked {
		rebased = append(rebased, filepath.Join(dir, "share", strings.TrimPrefix(g, "/usr/share/qcom/")))
	}
	qualcommDSPHostLockedGlobs = rebased
	qualcommDSPDeviceGlob = filepath.Join(dir, "dev", "fastrpc-*")

	t.Cleanup(func() {
		qualcommTransportGlobs, qualcommNPUStageDir = origTransport, origStage
		qualcommDSPHostLockedGlobs, qualcommDSPDeviceGlob = origLocked, origDevice
	})
	return dir
}

// stageFastrpcNode gives the tempdir a grantable transport node, without which every
// path here is inert by design.
func stageFastrpcNode(t *testing.T, dir, name string) {
	t.Helper()
	writeUnder(t, filepath.Join(dir, "dev"), name)
}

// stageTransport puts a library in the staged runtime, standing in for what
// EnsureQualcommNPURuntime produces at agent start.
func stageTransport(t *testing.T, dir, name string) {
	t.Helper()
	writeUnder(t, qualcommNPUStageDir, name)
}

// newQualcommSpec is newSpec with a Process, for the tests that assert on env.
func newQualcommSpec() *oci.Spec {
	spec := newSpec()
	spec.Process = &oci.Process{}
	return spec
}

func writeUnder(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// The contract is that the host provides only the driver-locked layer, so nothing may
// land in a path the image owns -- that is where shadowing and glibc coupling live.
func TestApplyQualcommNPURuntime_TouchesNoImageOwnedPath(t *testing.T) {
	dir := useQualcommRuntimeDir(t,
		"libcdsprpc.so.1.0.0",
		"libQnnHtp.so",  // framework: the app's job
		"genie-t2t-run", // tool: the app's job
		"share/dsp/cdsp/fastrpc_shell_unsigned_3",
	)
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	stageTransport(t, dir, "libcdsprpc.so.1.0.0")

	spec := newSpec()
	if result := ApplyQualcommNPURuntime(spec); !result.HasDSP {
		t.Fatal("hasDSP = false with a grantable fastrpc node staged")
	}

	for _, m := range spec.Mounts {
		if strings.HasPrefix(m.Destination, "/usr/lib") || strings.HasPrefix(m.Destination, "/usr/bin") {
			t.Errorf("mount into an image-owned path: %s", m.Destination)
		}
	}
}

// The transport is kernel-locked, so it must reach the container and win over a copy the
// image bundled -- at a prefix the image does not own.
func TestApplyQualcommNPURuntime_InjectsTransportAtOwnPrefix(t *testing.T) {
	dir := useQualcommRuntimeDir(t)
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	stageTransport(t, dir, "libcdsprpc.so.1.0.0")

	spec := newQualcommSpec()
	if result := ApplyQualcommNPURuntime(spec); !result.HasDSP || !result.TransportApplied || result.Mounts != 1 {
		t.Fatalf("result = %+v; want transport applied on a board with a DSP", result)
	}

	m, ok := mountForDest(spec, qualcommNPUPrefix)
	if !ok {
		t.Fatalf("expected a bind at %s, mounts = %+v", qualcommNPUPrefix, spec.Mounts)
	}
	if m.Source != qualcommNPUStageDir || !contains(m.Options, "ro") {
		t.Errorf("unexpected mount %+v", m)
	}

	var ldPath string
	for _, e := range spec.Process.Env {
		if strings.HasPrefix(e, "LD_LIBRARY_PATH=") {
			ldPath = e
		}
	}
	if !strings.Contains(ldPath, qualcommNPUPrefix) {
		t.Errorf("LD_LIBRARY_PATH = %q, want it to name %s", ldPath, qualcommNPUPrefix)
	}
}

// An app's own search path must survive, with ours taking precedence for the transport.
func TestApplyQualcommNPURuntime_PrependsToExistingLibraryPath(t *testing.T) {
	dir := useQualcommRuntimeDir(t)
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	stageTransport(t, dir, "libcdsprpc.so.1.0.0")

	spec := newQualcommSpec()
	spec.Process.Env = append(spec.Process.Env, "LD_LIBRARY_PATH=/app/lib")
	ApplyQualcommNPURuntime(spec)

	want := "LD_LIBRARY_PATH=" + qualcommNPUPrefix + ":/app/lib"
	var found bool
	for _, e := range spec.Process.Env {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Errorf("Env = %v, want %q", spec.Process.Env, want)
	}
}

// Only the board-locked artefacts are bound: the PD shells and the Hexagon C++ runtime,
// which the public SDK does not ship and no app can obtain. The QNN skels in the same
// directory come from that SDK, so they stay the app's to pin.
func TestApplyQualcommNPURuntime_BindsUnobtainableArtefactsOnly(t *testing.T) {
	dir := useQualcommRuntimeDir(t,
		"share/conf.d/board.yaml",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/fastrpc_shell_unsigned_3",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libc++.so.1",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libc++abi.so.1",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libQnnHtpV75Skel.so",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libsysmonquery_skel.so",
	)
	stageFastrpcNode(t, dir, "fastrpc-cdsp")

	spec := newSpec()
	if result := ApplyQualcommNPURuntime(spec); !result.HasDSP {
		t.Fatal("hasDSP = false with a grantable fastrpc node staged")
	}

	for _, name := range []string{
		"share/conf.d/board.yaml",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/fastrpc_shell_unsigned_3",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libc++.so.1",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libc++abi.so.1",
	} {
		if _, ok := mountForDest(spec, filepath.Join(dir, name)); !ok {
			t.Errorf("unobtainable artefact %s was not bound", name)
		}
	}

	for _, name := range []string{
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libQnnHtpV75Skel.so",
		"share/qcs8300/Qualcomm/EVK/dsp/cdsp/libsysmonquery_skel.so",
	} {
		if _, ok := mountForDest(spec, filepath.Join(dir, name)); ok {
			t.Errorf("%s ships in the public SDK or is unused; injecting it shadows the app", name)
		}
	}
}

// An empty stage dir means the host has no transport. Binding it anyway would put an
// empty directory on the app's library path and hide that from the caller.
func TestApplyQualcommNPURuntime_NoTransportBindWhenStageEmpty(t *testing.T) {
	dir := useQualcommRuntimeDir(t)
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	if err := os.MkdirAll(qualcommNPUStageDir, 0o755); err != nil {
		t.Fatalf("mkdir stage: %v", err)
	}

	spec := newQualcommSpec()
	result := ApplyQualcommNPURuntime(spec)
	if !result.HasDSP {
		t.Fatal("hasDSP = false with a grantable fastrpc node staged")
	}
	if result.Mounts != 0 {
		t.Fatalf("mounts = %d, want 0", result.Mounts)
	}
	if result.TransportApplied {
		t.Error("empty stage reported as applied transport")
	}
	if _, ok := mountForDest(spec, qualcommNPUPrefix); ok {
		t.Error("an empty stage dir must not be bound")
	}
}

// Board configuration and shells can exist even when transport staging failed.
// Their mounts must not suppress the caller's missing-transport warning.
func TestApplyQualcommNPURuntime_BoardFilesWithoutTransport(t *testing.T) {
	for _, emptyStage := range []bool{false, true} {
		name := "missing stage"
		if emptyStage {
			name = "empty stage"
		}
		t.Run(name, func(t *testing.T) {
			dir := useQualcommRuntimeDir(t,
				"share/conf.d/board.yaml",
				"share/qcs8300/Qualcomm/EVK/dsp/cdsp/fastrpc_shell_unsigned_3",
			)
			stageFastrpcNode(t, dir, "fastrpc-cdsp")
			if emptyStage {
				if err := os.MkdirAll(qualcommNPUStageDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			spec := newQualcommSpec()
			spec.Process.Env = []string{"LD_LIBRARY_PATH=/app/lib"}
			result := ApplyQualcommNPURuntime(spec)
			if !result.HasDSP || result.Mounts != 2 || result.TransportApplied {
				t.Fatalf("result = %+v; want board-file mounts but no transport", result)
			}
			if _, ok := mountForDest(spec, qualcommNPUPrefix); ok {
				t.Error("missing transport must not be mounted")
			}
			if len(spec.Process.Env) != 1 || spec.Process.Env[0] != "LD_LIBRARY_PATH=/app/lib" {
				t.Errorf("missing transport changed the library path: %v", spec.Process.Env)
			}
		})
	}
}

// The entitlement is documented as inert on a board with no usable FastRPC node.
func TestApplyQualcommNPURuntime_InertWithoutGrantableDevice(t *testing.T) {
	dir := useQualcommRuntimeDir(t, "share/conf.d/board.yaml")
	stageTransport(t, dir, "libcdsprpc.so.1.0.0")

	spec := newSpec()
	result := ApplyQualcommNPURuntime(spec)
	if result.HasDSP || result.Mounts != 0 || result.TransportApplied || len(spec.Mounts) != 0 {
		t.Fatalf("result = %+v; want nothing without a device", result)
	}

	// A -secure-only board is the same case: those nodes are never granted.
	stageFastrpcNode(t, dir, "fastrpc-cdsp-secure")
	spec = newSpec()
	if result = ApplyQualcommNPURuntime(spec); result.HasDSP || result.Mounts != 0 || result.TransportApplied {
		t.Fatalf("result = %+v; a -secure-only board grants nothing", result)
	}
}

func TestApplyQualcommNPURuntime_NoRuntimeOnHost(t *testing.T) {
	dir := useQualcommRuntimeDir(t)
	stageFastrpcNode(t, dir, "fastrpc-cdsp")

	spec := newSpec()
	result := ApplyQualcommNPURuntime(spec)
	if !result.HasDSP {
		t.Fatal("hasDSP = false with a grantable fastrpc node staged")
	}
	if result.Mounts != 0 || result.TransportApplied || len(spec.Mounts) != 0 {
		t.Fatalf("result = %+v, mounts = %d; want none with no runtime staged", result, len(spec.Mounts))
	}
}

func TestApplyQualcommNPURuntime_SkipsDuplicateOfExistingSpecEntry(t *testing.T) {
	dir := useQualcommRuntimeDir(t, "share/conf.d/board.yaml")
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	stageTransport(t, dir, "libcdsprpc.so.1.0.0")

	spec := newSpec()
	spec.Mounts = append(spec.Mounts, oci.Mount{
		Destination: qualcommNPUPrefix,
		Source:      "/somewhere/else",
		Type:        "bind",
	})

	result := ApplyQualcommNPURuntime(spec)
	if result.Mounts != 1 || result.TransportApplied {
		t.Fatalf("result = %+v; want one board-file mount and no transport applied over the existing mount", result)
	}
	m, _ := mountForDest(spec, qualcommNPUPrefix)
	if m.Source != "/somewhere/else" {
		t.Fatalf("existing mount was overwritten: %+v", m)
	}
}

// EnsureQualcommNPURuntime runs at agent boot, so an OS update refreshes the staged
// transport without redeploying anything.
func TestEnsureQualcommNPURuntime_StagesTransportAndIsIdempotent(t *testing.T) {
	dir := useQualcommRuntimeDir(t, "libcdsprpc.so.1.0.0")
	stageFastrpcNode(t, dir, "fastrpc-cdsp")

	for i := 0; i < 2; i++ {
		EnsureQualcommNPURuntime(zap.NewNop())
		staged := filepath.Join(qualcommNPUStageDir, "libcdsprpc.so.1.0.0")
		if _, err := os.Stat(staged); err != nil {
			t.Fatalf("run %d: %s not staged: %v", i, staged, err)
		}
	}
}

// A soname symlink must be recreated relative, because its absolute host target does not
// exist inside the container's bind.
func TestEnsureQualcommNPURuntime_RewritesSymlinksRelative(t *testing.T) {
	dir := useQualcommRuntimeDir(t, "libcdsprpc.so.1.0.0")
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	if err := os.Symlink(filepath.Join(dir, "libcdsprpc.so.1.0.0"), filepath.Join(dir, "libcdsprpc.so.1")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	EnsureQualcommNPURuntime(zap.NewNop())

	target, err := os.Readlink(filepath.Join(qualcommNPUStageDir, "libcdsprpc.so.1"))
	if err != nil {
		t.Fatalf("staged symlink missing: %v", err)
	}
	if target != "libcdsprpc.so.1.0.0" {
		t.Errorf("target = %q, want the basename so it resolves inside the bind", target)
	}
}

// Nothing is staged on a board with no DSP, so the prefix stays absent there.
func TestEnsureQualcommNPURuntime_InertWithoutDevice(t *testing.T) {
	useQualcommRuntimeDir(t, "libcdsprpc.so.1.0.0")

	EnsureQualcommNPURuntime(zap.NewNop())
	if _, err := os.Stat(qualcommNPUStageDir); !os.IsNotExist(err) {
		t.Errorf("stage dir was created on a board with no DSP")
	}
}

// A library dropped by an OS update must not linger, or the container's library path
// still resolves it.
func TestEnsureQualcommNPURuntime_PrunesStaleEntries(t *testing.T) {
	dir := useQualcommRuntimeDir(t, "libcdsprpc.so.1.0.0")
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	EnsureQualcommNPURuntime(zap.NewNop())

	stale := filepath.Join(qualcommNPUStageDir, "libgone.so.1")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	EnsureQualcommNPURuntime(zap.NewNop())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a staged library the host no longer offers must be pruned")
	}
	if _, err := os.Stat(filepath.Join(qualcommNPUStageDir, "libcdsprpc.so.1.0.0")); err != nil {
		t.Errorf("the current library must survive pruning: %v", err)
	}
}

// A source that cannot be read must leave the previously staged copy in place, rather
// than deleting it and reporting success.
func TestEnsureQualcommNPURuntime_KeepsCopyWhenSourceUnreadable(t *testing.T) {
	dir := useQualcommRuntimeDir(t, "libcdsprpc.so.1.0.0")
	stageFastrpcNode(t, dir, "fastrpc-cdsp")
	EnsureQualcommNPURuntime(zap.NewNop())

	staged := filepath.Join(qualcommNPUStageDir, "libcdsprpc.so.1.0.0")
	if err := os.Chmod(filepath.Join(dir, "libcdsprpc.so.1.0.0"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "libcdsprpc.so.1.0.0"), 0o644) })

	EnsureQualcommNPURuntime(zap.NewNop())

	if _, err := os.Stat(staged); err != nil {
		t.Errorf("staged copy was lost when the source became unreadable: %v", err)
	}
}
