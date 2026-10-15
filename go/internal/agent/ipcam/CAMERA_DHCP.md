# Direct camera DHCP opt-in

Camera DHCP is disabled by default. Operators configure it through the CLI:

```sh
wendy device camera dhcp status
wendy device camera dhcp enable eth1
wendy device camera dhcp disable eth1
```

Use `--device <device>` to select the WendyOS device and `--json` for structured
status. Enable only a dedicated wired camera port. The agent rejects an enable
request for a wireless, loopback, or missing interface, or one with conflicting
IPv4 configuration.
Enabled means permission to serve: the 15-second link scan and existing
unanswered-request and competing-server guards still control activation.
Status reports saved permission separately from serving, degraded, waiting, blocked,
missing, and unknown states. Disabling persists the change and cancels the
server before returning, even if interface inspection or address cleanup fails.

After an agent restart, an enabled port with exactly one `10.98.x.1/24`
address reuses that segment after the competing-server observation window.
This recovery does not depend on a camera having received a lease or being
recorded in the camera registry. An addressless port follows normal allocation;
other addresses or prefixes are reported as conflicting configuration. Allocation
excludes subnets overlapping any IPv4 network configured on another port,
including blocked or disabled ports and ports with multiple addresses.

Each subnet also has an agent-managed `camera-dhcp-leases-10.98.x.1.json`
file beside the configuration. MAC/IP reservations and their expiries are
saved with a file sync, atomic rename, and directory sync before a DHCP reply.
Deadlines use Linux boot elapsed time (including suspend) and the kernel boot
ID, so correcting the device's wall clock cannot expire a live lease. An agent
restart in the same boot preserves exact deadlines. After a device reboot,
old reservations are conservatively held until the new boot is 12 hours old;
restarting the agent again does not extend that deadline. This works offline,
without requiring time synchronization. Legacy wall-clock lease files are
converted once, conservatively reserving their addresses for another 12 hours.
Restarting, disabling, or releasing a port preserves unexpired reservations;
DHCP releases are persisted and expired reservations become available again.
Offers conservatively reserve an address for the advertised lease window too.
Replies advertise only the remaining persisted lifetime, excluding time spent
writing the file. Storage failures stop allocation and appear in status.
Requests confirm only the exact address reserved for that client. A conflicting
request is rejected; an unknown client checking a cached lease gets no reply
and must discover an offer before obtaining a new address. Requests selecting
another server are ignored.

When upgrading from an agent without lease files, known camera addresses are
reserved for one lease window (12 hours) on the first use of that subnet. This
migration is persisted once; loading the camera registry on later restarts does
not extend reservations. Multiple historical cameras can have the same last
address. Such ambiguous addresses are reserved without an owner for that window,
while other addresses remain available; neither historical camera can release
the ambiguous reservation early. Operators do not manage these files.

Permission follows the configured interface name. If an interface is renamed,
the next scan or status/control reconciliation stops its old server and watcher
and removes the owned address using its kernel interface index. Enable the new
name explicitly to resume. A missing camera address or inactive link is reported
as degraded; address restoration errors appear in the status detail and are
cleared after recovery.
Changing an active camera address's prefix from `/24` is a configuration conflict:
status reports it, and the next scan stops DHCP without replacing the new prefix.

The watcher detects competing DHCP replies and cancels serving without waiting
for configuration or lease storage I/O. Address cleanup is serialized with
scans, and watcher-generation checks reject callbacks from renamed ports.
Scans also finish pending withdrawals if the watcher exits before its queued
cleanup runs.

The agent manages `/etc/wendy-agent/camera-dhcp-interfaces` (or beneath
`WENDY_CONFIG_PATH`) internally, using atomic writes of a root-owned, mode-0600
file. File and directory synchronization completes before acknowledging success,
including newly created configuration directories. A failure after rename is
reported, and visible revocations still stop serving. Other ports' settings are
preserved. The parent directory must be
root-owned and not writable by group or others; symlinks and insecure files
fail closed. External edits are reread on each scan. On WendyOS the configuration
lives on the persistent `/data/etc/wendy-agent` bind mount and survives an OTA.
Operators do not need to create or edit this file. Passive camera observation
and ordinary network-camera discovery remain available with DHCP disabled.

An older agent may have left a stale `10.98.x.1/24` address or NetworkManager
profile on a former uplink. Disabling camera DHCP prevents new leases but does
not delete operator-managed profiles; remove those stale addresses/profiles
separately during migration.

The Linux lifecycle tests create temporary interfaces and must run only in an
isolated network namespace, as root, with `ip` installed and `NET_ADMIN` and
`NET_RAW` capabilities. Set `WENDY_TEST_NETNS=1` to opt in. The veth packet test
uses both ends in the same namespace, requiring
`net.ipv4.conf.all.accept_local=1`, `net.ipv4.conf.all.rp_filter=0`, and
`net.ipv4.conf.default.rp_filter=0`. Run
`go test -race ./go/internal/agent/ipcam -run '^TestDHCPLinux'` there.
