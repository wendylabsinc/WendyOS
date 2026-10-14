# Direct camera DHCP opt-in

Camera DHCP is disabled by default. Operators configure it through the CLI:

```sh
wendy device camera dhcp status
wendy device camera dhcp enable eth1
wendy device camera dhcp disable eth1
```

Use `--device <device>` to select the WendyOS device and `--json` for structured
status. Enable only a dedicated wired camera port. The agent rejects an enable
request for a wireless, loopback, missing, or already addressed uplink interface.
Enabled means permission to serve: the 15-second link scan and existing
unanswered-request and competing-server guards still control activation.
Status reports saved permission separately from serving, waiting, blocked,
missing, and unknown states. Disabling persists the change and cancels the
server before returning, even if interface inspection or address cleanup fails.

The agent manages `/etc/wendy-agent/camera-dhcp-interfaces` (or beneath
`WENDY_CONFIG_PATH`) internally, using atomic writes of a root-owned, mode-0600
file. Other ports' settings are preserved. The parent directory must be
root-owned and not writable by group or others; symlinks and insecure files
fail closed. External edits are reread on each scan. On WendyOS the configuration
lives on the persistent `/data/etc/wendy-agent` bind mount and survives an OTA.
Operators do not need to create or edit this file. Passive camera observation
and ordinary network-camera discovery remain available with DHCP disabled.

An older agent may have left a stale `10.98.x.1/24` address or NetworkManager
profile on a former uplink. Disabling camera DHCP prevents new leases but does
not delete operator-managed profiles; remove those stale addresses/profiles
separately during migration.
