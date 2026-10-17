# `wendy device unenroll`

Removes a device's enrollment from Wendy Cloud. Cloud mode works when the device
is offline or its firmware cannot run.

## Usage

```sh
wendy device unenroll [--device <selector>] [--cloud-only] [--cloud-grpc <endpoint>] [--yes]
wendy cloud device unenroll --device <asset-id-or-name> [--cloud-grpc <endpoint>] [--yes]
```

## Remove an offline device

Find the old asset, then remove it using its asset ID, name, or PKI device ID:

```sh
wendy cloud discover --all --json
wendy cloud device unenroll --device <asset-id-or-name>
```

For example, an enrollment conflict for `lite-441bf6804824` can be resolved with:

```sh
wendy cloud device unenroll --device lite-441bf6804824
```

`wendy device unenroll --cloud-only --device <asset-id-or-name>` performs the same
operation. Cloud mode authenticates with your stored cloud session, revokes the
device's certificates and bootstrap credentials, and deletes its cloud asset.
It releases the asset's device ID for another enrollment without connecting to
the device. Local enrollment credentials remain on the device.

## Remove a reachable device

```sh
wendy device unenroll --device <device-selector>
```

For a WendyOS agent with numeric Cloud v1 enrollment, this first verifies that
the user in the selected Cloud session is an owner or admin of the device's
organization, then lists every page of the device's certificates. A failed
check leaves the device enrolled and its Cloud records unchanged. Members,
viewers, billing managers, and users absent from the organization's membership
list cannot run this reset. Sign in as an owner or admin of that organization
to continue. If the session has no stored user ID, run `wendy auth login` again.

After these checks, the command clears the device's enrollment and provisioning
state, restarts the agent, and removes its cloud enrollment. Cleanup lists the
certificates again after reset before revoking them and deleting the asset.

If Cloud cleanup fails after the reset, the command reports the original Cloud,
organization and asset, plus a `wendy cloud device unenroll` command scoped to
that enrollment. Use that command to retry cleanup without contacting the reset
device. If the legacy Cloud API remains unavailable, use that Cloud dashboard
to remove the asset. Rerunning local unenrollment cannot recover the erased
enrollment identifiers.

For a WendyOS device with UUID/direct PKI enrollment, connect over the LAN
(direct mTLS, not a Cloud tunnel). Cloud revokes the device certificate and
deletes the asset; the device then verifies Cloud's signed reset authorization
and the certificate's revocation before erasing its credentials, and can enroll
again with the same device ID. If any check fails, the device keeps its keys
and the same command can be retried. Agents without Cloud-authenticated reset
support are refused before any Cloud change; update them with
`wendy device update` first. `--check` only verifies the Cloud binding and the
Agent's support. Use `--cloud-only` when you intend to remove only the Cloud
enrollment; this leaves local device credentials intact. See
[device unenrollment](../../../../device/unenrollment.mdx) and the
[unenrollment compatibility requirements](./enroll.md#unenrollment-compatibility).

For Wendy Lite, select the board's USB connection. The command checks firmware
reset support before changing Cloud, revokes and deletes the matching cloud
enrollment, then clears the local enrollment identity and reboots the board.
Wi-Fi and app configuration are preserved. Firmware without reset support is
rejected before cloud deletion and must be updated first. LAN and Bluetooth
cannot perform this reset; use `--cloud-only` for cloud cleanup alone.

If the cloud asset was already deleted, including by an earlier `--cloud-only`
command, rerun normal unenrollment over USB. It checks the selected cloud
organization's inventory before clearing the remaining local identity. Cloud
authentication and lookup failures stop the reset. If local reset fails after
cloud cleanup, the error reports that partial result and the same command can
be retried. After successful reset, the board can enroll again.

A `cloud://` selector supplied through `--device` or the saved default uses cloud
removal directly, without opening a device connection.

## Flags

| Flag | Description |
|------|-------------|
| `--device` | Device selector. In cloud mode, accepts a cloud asset ID, name, or PKI device ID. |
| `--cloud-only` | Remove the cloud enrollment without connecting to the device. Available on `wendy device unenroll`. |
| `--cloud-grpc` | Cloud gRPC endpoint for the stored session used to remove the enrollment. |
| `--asset-id` | Exact Cloud asset UUID for a WendyOS v2 device; must match its Cloud binding. Unavailable for cloud mode and Wendy Lite. |
| `--check` | Verify the Cloud binding and the Agent's authenticated-reset support without changing anything. Unavailable for cloud mode and Wendy Lite. |
| `--yes` | Skip confirmation. Required when running non-interactively. |

## Related

- [`wendy device enroll`](./enroll.md)
- [Wendy Lite enrollment](../../../../cli/lite-enrollment.md)
