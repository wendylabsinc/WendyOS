# Direct camera DHCP opt-in

The agent passively observes DHCP requests on addressless wired interfaces and
discovers/probes IP cameras on ordinary networks. It serves DHCP only on
interfaces named in `/etc/wendy-agent/camera-dhcp-interfaces` (or the same
filename beneath `WENDY_CONFIG_PATH` when that override is set). The default is
an absent file and **no camera DHCP servers**. This prevents an uplink that is
temporarily addressless from offering a camera subnet to a LAN client.

For a directly cabled camera on `eth1`, an administrator creates a regular,
root-owned file (for example, mode `0600`) containing one exact interface name
per line:

```text
# Direct camera cable
eth1
```

The parent directory must also be root-owned and not writable by group or
others. Symlinks, invalid names, and insecure files fail closed. The agent
rereads the file on every 15-second link scan. Removing a name withdraws its
active camera DHCP server and segment address on the next scan. The existing
unanswered-request and competing-server guards still apply to opted-in ports.
The allowlist lives on the persistent `/data/etc/wendy-agent` bind mount on
WendyOS devices, so it survives an OTA.

An older agent may have left a stale `10.98.x.1/24` address or NetworkManager
profile on a former uplink. Disabling camera DHCP prevents new leases but does
not delete operator-managed profiles; remove those stale addresses/profiles
separately during migration.
