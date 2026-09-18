package services

import (
	"context"
	"fmt"
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
		logger.Warn("container storage gate disabled: not a WendyOS host")
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
// via `systemctl show -p LoadState --value <unit>`. Any systemctl failure,
// or a LoadState other than "loaded" (e.g. "not-found" for an absent unit),
// reports false.
func bindMountUnitLoaded(unit string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := systemctlFn(ctx, "show", "-p", "LoadState", "--value", unit)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "loaded"
}
