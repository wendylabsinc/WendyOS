Selects a device as the [default device](../../device-selection.md), so that other commands default to this device if available.

The default can be a hostname, IP address, provider key, or explicit `host:port` value:

```sh
wendy device set-default my-mac.local:50051
wendy device info --json
wendy run
```

Use `wendy device get-default` to see the current default, or `wendy device unset-default` to clear it.

A device name made only of digits is rejected. To make a cloud asset the default, pass its cloud selector, `cloud://<cloud-grpc-host:port>/org/<org-id>/asset/<asset-id>`; the error for a numeric name prints the exact selector when you are logged in.

## Scripts and other non-interactive shells

With no argument, `set-default` opens the device picker, which needs an interactive terminal. In a script, CI, an AI agent's shell, or with `--json`, it instead exits with an error that shows the command to run and the devices this CLI has seen recently:

```sh
wendy device set-default wendyos-calm-zinnia.local
wendy device set-default vm:dev
```

To target a different device in one shell or AI session without changing the default that every session shares, set `WENDY_DEVICE` instead (see [device selection](../../device-selection.md)).

## Certificate pinning

An enrolled device's certificate must match a trusted expected device identity,
including its organisation and asset or device ID. The CLI gets that identity
from authenticated Cloud inventory, verified enrollment, or an existing saved
binding. Older organisation/asset certificate identities are still supported.
See [device identity](../../device-selection.md#device-identity) for connection
requirements and recovery steps.

Setting a LAN or VM default clears its saved pin and attempts a connection.
The default is saved even if that connection cannot be established. Clearing
the pin does not let the CLI learn a new identity from whichever device answers:
an enrolled-device connection still needs a trusted expected identity. Cloud
selectors use the selected inventory identity and have no LAN hostname pin to
clear.

Routine certificate renewal is accepted when the device identity stays the
same. A different organisation, issuing Cloud, asset, or device identity is
refused. A previously pinned device that no longer presents an enrolled
identity is also refused. These checks apply in interactive, `--json`, and
non-interactive runs, without a trust override prompt.

After an intentional replacement or reset, you can clear a pin without
changing the default:

```sh
wendy device unpin <host>
```

Before reconnecting to an enrolled device, refresh its trusted Cloud inventory
binding. For a reset device that is now unprovisioned, complete explicit
`wendy device enroll` instead. If the identity change was unexpected, keep the
pin and verify the device first.

Loopback addresses share the pin of their bare host, whatever the port: `127.0.0.1:50051` and `127.0.0.1:50061` are both checked against the `127.0.0.1` pin, and `localhost:50051` against the `localhost` pin. The exception is a running local VM: `127.0.0.1` on its forwarded agent port, or on the mTLS port after it, is that VM, pinned as `vm:<name>` like `--device vm:<name>`, so each VM keeps its own pin. `wendy device set-default vm:<name>` clears only that VM's pin, never the shared `127.0.0.1` pin.
