# Fix Windows Discovery Issues

## Context

While testing Wendy on Windows with a Jetson Orin Nano freshly installed with WendyOS and connected over USB, `wendy discover` initially failed to show the device on Windows. The same Jetson did show up when connected to a Mac and discovered there.

The Windows host sees the Jetson as a network device:

- Adapter description: `UsbNcm Host Device`
- Interface alias: `Ethernet`
- Interface index: `7`
- Status: `Up`
- Windows USB-side IPv4: `169.254.167.228/16`
- Windows USB-side IPv6: `fe80::11f6:5a69:a4f2:9f48%7`
- Network profile was initially `Public`

`wendy discover --type usb` is not expected to find this device because the Jetson appears as LAN-over-USB / USB-NCM, not as a plain USB PnP Wendy device.

## Observed Failure

These commands initially returned no Jetson devices on Windows:

```powershell
go run ./cmd/wendy discover --type lan --timeout 30s --json
go run ./cmd/wendy discover --type lan --timeout 60s --json
go run ./cmd/wendy discover --timeout 60s --json
go run ./cmd/wendy discover --type usb --timeout 30s --json
```

The all-transports command only returned the local external provider:

```json
{
  "usbDevices": null,
  "lanDevices": null,
  "bluetoothDevices": null,
  "ethernetDevices": null,
  "externalDevices": [
    {
      "id": "local",
      "displayName": "Local Machine",
      "providerKey": "local",
      "isWendyDevice": false,
      "os": "windows",
      "cpuArchitecture": "amd64"
    }
  ]
}
```

## Diagnostics

A temporary Go mDNS probe using `github.com/hashicorp/mdns` was copied to the Windows machine and run from the repo's `go` directory.

The probe printed Go-visible interfaces including:

```text
ifIndex=7 name="Ethernet" flags=up|broadcast|multicast|running addrs=[fe80::11f6:5a69:a4f2:9f48/64 169.254.167.228/16]
```

The default/all-interface mDNS query returned zero results:

```text
=== mDNS query: default/all interfaces disable4=false disable6=false ===
DONE count=0
```

But explicitly querying interface 7 found the Jetson immediately:

```text
=== mDNS query: interface 7 Ethernet both disable4=false disable6=false ===
FOUND name="wendyos-prudent-lark._wendyos._udp.local." host="wendyos-prudent-lark.local." port=50051 addr4=<nil> addr6=fe80::576f:1b86:d80b:a8b9 txt=[id=9b053f53-3dcf-44cc-b391-e803b4a8d8f6 name=prudent-lark displayname=Prudent Lark]
DONE count=1
```

IPv4-only on that interface also found it and provided IPv4:

```text
=== mDNS query: interface 7 Ethernet ipv4-only disable4=false disable6=true ===
FOUND name="wendyos-prudent-lark._wendyos._udp.local." host="wendyos-prudent-lark.local." port=50051 addr4=169.254.249.48 addr6=fe80::576f:1b86:d80b:a8b9 txt=[id=9b053f53-3dcf-44cc-b391-e803b4a8d8f6 name=prudent-lark displayname=Prudent Lark]
DONE count=1
```

Direct connectivity works:

```text
ComputerName     : 169.254.249.48
RemoteAddress    : 169.254.249.48
RemotePort       : 50051
InterfaceAlias   : Ethernet
SourceAddress    : 169.254.167.228
TcpTestSucceeded : True
```

After explicit interface probing, normal Wendy LAN discovery began to find the device:

```json
{
  "lanDevices": [
    {
      "id": "9b053f53-3dcf-44cc-b391-e803b4a8d8f6",
      "displayName": "wendyos-prudent-lark",
      "hostname": "wendyos-prudent-lark.local",
      "ipAddress": "169.254.249.48",
      "port": 50051,
      "interfaceType": "lan",
      "isWendyDevice": true,
      "agentVersion": "2026.05.04-145708",
      "deviceType": "jetson-orin-nano-devkit-nvme-wendyos",
      "os": "linux",
      "cpuArchitecture": "arm64"
    }
  ]
}
```

After clearing the ARP entry, discovery could still find the device but sometimes returned only IPv6:

```json
{
  "lanDevices": [
    {
      "displayName": "wendyos-prudent-lark",
      "hostname": "wendyos-prudent-lark.local",
      "ipAddress": "fe80::576f:1b86:d80b:a8b9",
      "port": 50051
    }
  ]
}
```

This suggests Windows discovery should handle per-interface mDNS and IPv6 link-local zones robustly, and should prefer IPv4 when available.

## Current Code Notes

Windows LAN discovery is in:

```text
go/internal/shared/discovery/discovery_windows.go
```

It currently runs one `hashicorp/mdns` query using `mdns.DefaultParams(wendyServiceType)` and does not enumerate interfaces.

Windows Ethernet discovery is stubbed:

```go
func discoverEthernet(_ context.Context) ([]models.EthernetInterface, error) {
    return nil, nil
}
```

Linux has a more robust fallback in:

```text
go/internal/shared/discovery/discovery_linux.go
```

Specifically `discoverLANMDNS` and `queryInterface` enumerate interfaces and run mDNS per interface:

- `net.Interfaces()`
- skip down interfaces
- skip non-multicast interfaces
- skip loopback
- `params.Interface = iface`
- deduplicate results
- add zone to IPv6 link-local addresses

The Windows implementation should likely mirror this logic.

## Likely Root Cause

On Windows, the default/all-interface `hashicorp/mdns` query does not reliably discover `_wendyos._udp` advertisements on the USB-NCM interface. Explicitly querying the USB-NCM interface works.

This makes cold `wendy discover` unreliable for Jetson devices connected via USB-NCM on Windows.

## Proposed Direction

Flesh out and implement a plan to make Windows discovery reliable:

1. Update `discoverLAN` in `go/internal/shared/discovery/discovery_windows.go` to query mDNS per interface, similar to Linux's `discoverLANMDNS`.
2. Enumerate all `net.Interfaces()`.
3. Skip interfaces that are not up, do not support multicast, or are loopback.
4. For each eligible interface, call a Windows version of `queryInterface` with `params.Interface = &iface`.
5. Deduplicate devices by service name / hostname / port.
6. Parse TXT records consistently:
   - `id`
   - maybe also `wendyosdevice` if present, matching Linux/macOS behavior
   - `displayname`
   - `tls`
7. Prefer IPv4 when available.
8. For IPv6 link-local addresses, append the zone/interface so follow-up connections are routable on Windows.
9. Ensure `resolveLANVersions` can connect using whatever address is returned.
10. Add tests around converting mDNS service entries to `models.LANDevice` so parsing/address selection behavior is covered without needing a real Windows mDNS environment.

## Validation Commands

On Windows, from the repo's `go` directory:

```powershell
go run ./cmd/wendy discover --type lan --timeout 10s --json
go run ./cmd/wendy discover --timeout 10s --json
go run ./cmd/wendy discover --type usb --timeout 10s --json
```

Expected LAN result should include the Jetson over USB-NCM, e.g.:

```json
{
  "displayName": "wendyos-prudent-lark",
  "hostname": "wendyos-prudent-lark.local",
  "ipAddress": "169.254.249.48",
  "port": 50051,
  "interfaceType": "lan",
  "isWendyDevice": true,
  "deviceType": "jetson-orin-nano-devkit-nvme-wendyos",
  "cpuArchitecture": "arm64"
}
```

Also validate cold-start behavior after unplug/replug or clearing neighbor/ARP state.

## Implementation Plan

Keep this fix focused on the Windows discovery path only. Do not refactor
Linux/macOS discovery as part of this change.

1. Preserve the existing default Windows mDNS query as a source of candidates.
   This keeps current behavior intact and reduces regression risk.
2. Add a Windows-only per-interface mDNS query path:
   - enumerate `net.Interfaces()`
   - skip interfaces that are down, non-multicast, or loopback
   - call `mdns.Query` with `params.Interface = &iface`
3. Collect candidates from the default query and all per-interface queries into
   one slice first.
4. Run a separate final deduplication pass. Keep it simple:
   - key primarily by discovered device ID when available
   - otherwise fall back to hostname/port or display/hostname/port
   - prefer IPv4 over IPv6 when merging duplicate candidates
   - prefer non-empty addresses and richer metadata over sparse candidates
5. Keep address handling minimal but robust:
   - prefer `AddrV4` when present
   - otherwise use `AddrV6`
   - append the interface zone for IPv6 link-local addresses returned from a
     per-interface query, so follow-up connections are routable on Windows
6. Parse TXT records consistently enough for Wendy devices:
   - `id`
   - `wendyosdevice` as an alternate ID if present
   - `displayname`
   - `tls=true`
7. Treat discovery as best-effort:
   - if interface enumeration fails, return default-query results
   - if one interface query fails, ignore it and continue
8. Add focused tests around the Windows conversion/deduplication helpers where
   possible without requiring a real mDNS environment:
   - IPv4 is preferred over IPv6
   - IPv6 link-local addresses get a zone when an interface is known
   - TXT fields populate ID/display name/mTLS
   - deduplication keeps the better candidate

## Task for Follow-up Session

Implement and test the Windows per-interface mDNS discovery fix following the
KISS plan above.
