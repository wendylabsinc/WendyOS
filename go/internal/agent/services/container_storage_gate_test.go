package services

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestContainerStorageGateNoopOffWendyOS asserts a host that
// defaultIsWendyOSHost() reports as not WendyOS is never gated, even when the
// probe reports the containerd storage is on root: generic Linux and macOS
// hosts legitimately run containerd on /.
func TestContainerStorageGateNoopOffWendyOS(t *testing.T) {
	g := newContainerStorageGate(zap.NewNop(),
		func() bool { return false },                                     // isWendyOS
		func(string) bool { return true },                                // unitLoaded
		func() bool { return true },                                      // probe
		func() (partitionUsage, bool) { return partitionUsage{}, false }, // describeUsage
	)

	if g.Degraded() {
		t.Fatal("gate must be a no-op off WendyOS")
	}
	if err := g.Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

// TestContainerStorageGateNoopWithoutBindUnit asserts a WendyOS variant
// without the var-lib-containerd.mount unit (e.g. qemu) is never gated:
// running containerd on / is that variant's normal configuration, not a
// failure.
func TestContainerStorageGateNoopWithoutBindUnit(t *testing.T) {
	g := newContainerStorageGate(zap.NewNop(),
		func() bool { return true },
		func(string) bool { return false },
		func() bool { return true },
		func() (partitionUsage, bool) { return partitionUsage{}, false },
	)

	if g.Degraded() {
		t.Fatal("gate must be a no-op without the bind-mount unit loaded")
	}
	if err := g.Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

// TestContainerStorageGateRefusesWhenOnRootAtStartup asserts the startup
// snapshot sticks: once containerd has opened its database on the root
// filesystem, a bind mount reappearing later does not un-gate the agent,
// because containerd is still writing to the rootfs inode underneath it.
func TestContainerStorageGateRefusesWhenOnRootAtStartup(t *testing.T) {
	calls := 0
	probe := func() bool {
		calls++
		return calls == 1 // true at construction, false afterwards
	}
	g := newContainerStorageGate(zap.NewNop(),
		func() bool { return true },
		func(string) bool { return true },
		probe,
		func() (partitionUsage, bool) { return partitionUsage{}, false },
	)

	if !g.Degraded() {
		t.Fatal("startup snapshot on root must stick even after the bind mount reappears")
	}
	if err := g.Check(); err == nil {
		t.Fatal("Check() = nil, want an error")
	}
}

// TestContainerStorageGateRefusesWhenOnRootNow asserts the per-call probe
// catches the bind mount vanishing after a clean startup.
func TestContainerStorageGateRefusesWhenOnRootNow(t *testing.T) {
	calls := 0
	probe := func() bool {
		calls++
		return calls > 1 // false at construction, true afterwards
	}
	g := newContainerStorageGate(zap.NewNop(),
		func() bool { return true },
		func(string) bool { return true },
		probe,
		func() (partitionUsage, bool) { return partitionUsage{}, false },
	)

	if !g.Degraded() {
		t.Fatal("per-call probe must catch the bind mount vanishing after startup")
	}
	if err := g.Check(); err == nil {
		t.Fatal("Check() = nil, want an error")
	}
}

// TestContainerStorageGateNilIsNoop asserts every method is safe to call on a
// nil *ContainerStorageGate, so consumers can hold an optional gate without
// nil-checking before every call.
func TestContainerStorageGateNilIsNoop(t *testing.T) {
	var g *ContainerStorageGate

	if g.Degraded() {
		t.Fatal("nil gate must report Degraded() == false")
	}
	if err := g.Check(); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	_ = g.Message() // must not panic
}

// TestContainerStorageGateMessageNamesPartitionAndRemedy asserts Message()
// names the backing device and its usage, states the remedy, and cites
// WDY-3127; and that Check() surfaces the identical text as a
// FailedPrecondition status error.
func TestContainerStorageGateMessageNamesPartitionAndRemedy(t *testing.T) {
	g := newContainerStorageGate(zap.NewNop(),
		func() bool { return true },
		func(string) bool { return true },
		func() bool { return true },
		func() (partitionUsage, bool) {
			return partitionUsage{device: "/dev/nvme0n1p1", usedBytes: 11_900_000_000, totalBytes: 12_000_000_000}, true
		},
	)

	msg := g.Message()
	for _, want := range []string{
		"/dev/nvme0n1p1",
		"11.9 GB of 12.0 GB used",
		"the /data bind mount is not active",
		"refusing to ingest images so the root filesystem cannot fill up",
		"Power-cycle the device",
		"(WDY-3127)",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Message() = %q, want it to contain %q", msg, want)
		}
	}

	err := g.Check()
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Check() code = %v, want FailedPrecondition", status.Code(err))
	}
	if got := status.Convert(err).Message(); got != msg {
		t.Fatalf("Check() message = %q, want it to equal Message() = %q", got, msg)
	}
}

// TestContainerStorageGateMessageOmitsUsageWhenUnknown asserts the usage
// parenthetical is dropped entirely, including its surrounding space, when
// describeUsage cannot report a partition — the message must still read as
// a well-formed sentence.
func TestContainerStorageGateMessageOmitsUsageWhenUnknown(t *testing.T) {
	g := newContainerStorageGate(zap.NewNop(),
		func() bool { return true },
		func(string) bool { return true },
		func() bool { return true },
		func() (partitionUsage, bool) { return partitionUsage{}, false },
	)

	msg := g.Message()
	if strings.Contains(msg, " GB") {
		t.Fatalf("Message() = %q, want no usage parenthetical when usage is unknown", msg)
	}
	if strings.Contains(msg, "  ") {
		t.Fatalf("Message() = %q, want no leftover double space where the parenthetical would have been", msg)
	}
	if !strings.Contains(msg, "resolves to / because the /data bind mount is not active") {
		t.Fatalf("Message() = %q, want a well-formed sentence without the usage parenthetical", msg)
	}
}

// TestBindMountUnitLoadedUsesSystemctl exercises the real systemctl-backed
// helper (overriding systemctlFn the way inhibit_updater_test.go does),
// asserting the exact invocation and its LoadState interpretation.
func TestBindMountUnitLoadedUsesSystemctl(t *testing.T) {
	orig := systemctlFn
	t.Cleanup(func() { systemctlFn = orig })

	var gotArgs []string
	systemctlFn = func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("loaded\n"), nil
	}
	if !bindMountUnitLoaded(containerStorageBindUnit) {
		t.Fatal(`bindMountUnitLoaded() = false, want true for systemctl output "loaded\n"`)
	}
	for _, want := range []string{"show", "-p", "LoadState", "--value", "var-lib-containerd.mount"} {
		if !slices.Contains(gotArgs, want) {
			t.Fatalf("systemctl args = %v, want them to include %q", gotArgs, want)
		}
	}

	systemctlFn = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("not-found"), nil
	}
	if bindMountUnitLoaded(containerStorageBindUnit) {
		t.Fatal(`bindMountUnitLoaded() = true, want false for systemctl output "not-found"`)
	}

	systemctlFn = func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, errors.New("boom")
	}
	if bindMountUnitLoaded(containerStorageBindUnit) {
		t.Fatal("bindMountUnitLoaded() = true, want false when systemctl errors")
	}
}
