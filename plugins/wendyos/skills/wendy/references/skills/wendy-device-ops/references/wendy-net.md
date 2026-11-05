# Reach devices through Wendy Net

For a device that is reachable only through the mesh, use a terminal on the
user's intended computer to sign in and join:

```bash
wendy auth login
wendy device wendy-net connect
wendy discover --json --type mesh --timeout 5s
wendy --device mesh://TENANT_UUID/DEVICE_UUID device info
```

Use the exact `mesh://...` destination returned by discovery. The CLI discovers
nearby routers over LAN and Bluetooth automatically; do not ask the user for a
router address, router UUID, Bluetooth identifier or PSM. Supported WendyOS and
PSRAM Lite routers can connect the computer; a C6 is a destination leaf, not an
entry router. A Lite destination supports its own management commands, not
WendyOS-only container operations.

With local MCP, `device_list` can list mesh destinations while the local
background router is connected. Use `device_connect(device=...)` for a returned
WendyOS mesh selector. `scan=true` adds ordinary LAN discovery; it does not
enable mesh participation. Use the CLI for Lite operations not exposed by the
connected MCP server. If no terminal is available on the intended computer,
explain the missing access rather than inventing a mesh-join tool.

`device_disconnect` ends the current MCP device connection. To stop the local
mesh router itself, use `wendy device wendy-net disconnect` when requested.
After that, discovery and device commands must leave it disabled until the user
explicitly reconnects with `wendy device wendy-net connect`. Ordinary LAN device
management remains available.
