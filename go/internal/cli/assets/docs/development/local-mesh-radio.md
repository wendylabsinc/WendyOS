# Connect Wendy Lite to Wendy Net over Bluetooth

Connect an enrolled Wendy Lite board to a nearby WendyOS device without joining
a Wi-Fi access point. The WendyOS device carries the board's Wendy Net traffic
onward. The Lite board is an endpoint: it does not relay traffic for other devices.

This is an advanced setup that requires compatible Lite firmware and explicit
peer configuration. For ordinary WendyOS-to-WendyOS Bluetooth links, use
[Wendy Net over Bluetooth](/docs/hardware/bluetooth#wendy-net-over-bluetooth).

## Before you start

- Enroll both devices in the same Wendy Cloud organization.
- Use a WendyOS device with Bluetooth and full Wendy Net support.
- Ask your firmware administrator for the board's enrolled device UUID,
  Bluetooth address, address type, and signed-time service URL.
- The Lite firmware must enable Bluetooth networking, trust the time service,
  and be configured with the WendyOS device's enrolled UUID.

Automatic Lite peer discovery and rotating Bluetooth addresses are not supported.

## Configure the WendyOS device

Merge the following fields into its existing
`/etc/wendy-agent/local-mesh.json`, preserving the other settings:

```json
{
  "leafBLEPeers": [{
    "device": "12345678-9abc-4def-8123-000000000002",
    "address": "80:45:6b:00:00:01",
    "addressType": "public",
    "psm": 129
  }],
  "leafTimeURL": "https://your-time-service.example/timestamp"
}
```

Replace all example values. `device` is the Lite board's enrolled UUID, not its
hardware ID. Use a different UUID for each board. Keep the file and its directory
owned by the agent's system user and unwritable by other users. Restart
`wendyos-agent.service` after editing.

## Check the connection

```sh
wendy device wendy-net status --device <wendyos-device>
wendy device wendy-net topology --device <wendyos-device>
```

Check for an authenticated link to the Lite board. A running Bluetooth provider
alone does not mean the board connected. On a cold boot, allow at least two
minutes for the board's replay protection before expecting normal traffic.
If it does not connect, check the configured identities, Bluetooth address,
firmware, and access to the signed-time service.

Lite connections share the adapter's capacity with other Wendy Net Bluetooth
links. Bluetooth is intended for low-bandwidth traffic; use Ethernet or Wi-Fi
where available.

## Advanced configuration reference

| Field | Meaning |
| --- | --- |
| `leafBLEPeers` | Up to eight explicitly configured Lite boards; physical Bluetooth connection limits still apply. |
| `addressType` | `public` (default) or `random`. Each address/type pair must be unique. |
| `psm` | Bluetooth service port, 128–255; defaults to 129. Keep 128 free if WendyCom management uses it. The firmware must use the same value. |
| `leafTimeURL` | Required HTTPS signed-time URL when Lite peers are configured. Credentials, query strings and fragments are not accepted. |

For custom firmware, enable `WENDY_MESH_BLE`, reserve at least two NimBLE
connection-oriented channels, and set `WENDY_MESH_ROUTER_DEVICE` to the WendyOS
device UUID. These are firmware settings, not Wendy CLI options. A Bluetooth
address locates a board; enrollment authenticates it.
