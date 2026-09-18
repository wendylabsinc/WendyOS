package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// containerStorageBindUnit is the systemd unit that binds /var/lib/containerd
// to /data on WendyOS hosts that expect it (var-lib-containerd.mount).
const containerStorageBindUnit = "var-lib-containerd.mount"

// ContainerStorageGate refuses image ingestion when /var/lib/containerd is
// being served from the OS root slot on a WendyOS host whose /data bind
// mount is inactive (WDY-3127). On hosts where that configuration is not
// expected — non-WendyOS hosts, and WendyOS variants without /data such as
// qemu — the gate is a permanent no-op: those hosts legitimately run
// containerd on /.
//
// A nil *ContainerStorageGate is itself a no-op, so consumers can hold an
// optional gate without nil-checking before every call.
type ContainerStorageGate struct {
	expected      bool
	startupOnRoot bool
	probe         func() bool
	describeUsage func() (partitionUsage, bool)
}

// NewContainerStorageGate probes the host once at construction — the agent
// starts after containerd, so this snapshot reflects the filesystem
// containerd actually opened its database on — and logs one Warn line
// explaining why it disabled itself when the host is not expected to have a
// /data bind mount at all.
func NewContainerStorageGate(logger *zap.Logger) *ContainerStorageGate {
	return newContainerStorageGate(logger, defaultIsWendyOSHost, bindMountUnitLoaded, containerStorageOnRootSlot, containerStorageUsage)
}

// newContainerStorageGate is the testable constructor: callers inject every
// collaborator so tests can drive isWendyOS/unitLoaded/probe/describeUsage
// independently of the host.
func newContainerStorageGate(
	logger *zap.Logger,
	isWendyOS func() bool,
	unitLoaded func(string) bool,
	probe func() bool,
	describeUsage func() (partitionUsage, bool),
) *ContainerStorageGate {
	g := &ContainerStorageGate{probe: probe, describeUsage: describeUsage}

	if !isWendyOS() {
		// Expected on every non-WendyOS agent (plain Ubuntu, macOS): not
		// worth an operator's attention, so Info rather than Warn (WDY-3127
		// M5). Warn is reserved below for a WendyOS host that unexpectedly
		// lacks the bind-mount unit.
		logger.Info("container storage gate disabled: not a WendyOS host")
		return g
	}
	if !unitLoaded(containerStorageBindUnit) {
		logger.Warn("container storage gate disabled: bind mount unit not loaded",
			zap.String("unit", containerStorageBindUnit))
		return g
	}

	g.expected = true
	g.startupOnRoot = probe()
	return g
}

// Degraded reports whether /var/lib/containerd is currently served from the
// OS root slot on a host expected to bind-mount /data. It is sticky once
// true at startup: a bind mount reappearing after containerd has already
// opened its database on the root filesystem does not clear it, because
// containerd is still writing to the rootfs inode underneath the new mount.
//
// A running app's container keeps writing to its rootfs upper dir on /
// while degraded regardless — StartContainer is intentionally not gated,
// only ingestion is. And if the bind is re-activated by hand while
// containerd still holds meta.db open on the rootfs inode, an agent restart
// re-snapshots as healthy even though containerd's own state is still on
// root. The OS-side RequiresMountsFor (Part A) is what closes off the
// realistic route to that state, by making containerd itself wait for the
// bind mount before it ever opens meta.db on root.
func (g *ContainerStorageGate) Degraded() bool {
	if g == nil || !g.expected {
		return false
	}
	return g.startupOnRoot || g.probe()
}

// Check returns a FailedPrecondition status error describing the degraded
// state, or nil when the gate is not degraded (including when it is a
// no-op or nil).
func (g *ContainerStorageGate) Check() error {
	if !g.Degraded() {
		return nil
	}
	return status.Error(codes.FailedPrecondition, g.Message())
}

// Message describes the degraded state and the remedy. It names the
// backing device and its usage when describeUsage can report them, and
// omits that parenthetical (including its surrounding space) otherwise.
func (g *ContainerStorageGate) Message() string {
	usage := ""
	if g != nil && g.describeUsage != nil {
		if p, ok := g.describeUsage(); ok {
			usage = fmt.Sprintf(" (%s, %s of %s used)", p.device, formatGigabytesOneDecimal(p.usedBytes), formatGigabytesOneDecimal(p.totalBytes))
		}
	}
	return "container storage is on the OS root slot: /var/lib/containerd resolves to /" + usage +
		" because the /data bind mount is not active; refusing to ingest images so the root filesystem cannot fill up." +
		" Power-cycle the device; if 'wendy device info' still shows container storage on /, the OS did not mount /data (WDY-3127)."
}

// formatGigabytesOneDecimal renders a byte count as a decimal gigabyte
// figure with exactly one fractional digit (e.g. "11.9 GB"), matching the
// wording used in Message().
func formatGigabytesOneDecimal(n int64) string {
	return fmt.Sprintf("%.1f GB", float64(n)/1_000_000_000)
}

// bindMountUnitLoaded reports whether systemd has the given unit loaded,
// via `systemctl show -p LoadState --value <unit>`. A clean answer other
// than "loaded" (e.g. "not-found" for an absent unit, "masked") reports
// false — that's a real answer, not a failure. When systemctl itself
// errors or times out, this is the one moment a false negative matters
// most: it would silently disable the gate on a host that actually needs
// it. So instead it falls back to checking whether the unit file exists on
// disk (WDY-3127 M6).
func bindMountUnitLoaded(unit string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := systemctlFn(ctx, "show", "-p", "LoadState", "--value", unit)
	if err != nil {
		return unitFileExists(unit)
	}
	return strings.TrimSpace(string(out)) == "loaded"
}

// unitFileStatFn is os.Stat, overridable in tests.
var unitFileStatFn = os.Stat

// systemdUnitDirs are systemd's two standard system-unit directories,
// checked in order by unitFileExists.
var systemdUnitDirs = []string{"/etc/systemd/system", "/lib/systemd/system"}

// unitFileExists is bindMountUnitLoaded's systemctl-failure fallback: it
// reports whether unit's file exists under either of systemd's standard
// system-unit directories.
func unitFileExists(unit string) bool {
	for _, dir := range systemdUnitDirs {
		if _, err := unitFileStatFn(dir + "/" + unit); err == nil {
			return true
		}
	}
	return false
}
