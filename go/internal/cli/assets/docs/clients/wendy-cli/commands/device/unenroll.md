# `wendy device unenroll`

Remove a device from Wendy Cloud and, when connected directly, clear its local
enrollment so it can enroll again. Sign in to the organization that owns it.

## Remove a reachable device

For WendyOS, use a direct LAN address. With a current enrollment, check support
first, then confirm the removal:

```sh
wendy device unenroll --device <device-address> --check
wendy device unenroll --device <device-address>
```

Update the agent with `wendy device update` if the command asks you to.
The native Mac agent does not yet support this reset for current enrollments.

For Lite, connect USB and use its selector from `wendy discover`:

```sh
wendy device unenroll --device wendy-lite:/dev/cu.usbmodemXXXX
```

Lite reboots after removal and keeps its Wi-Fi and app settings. Firmware must
support enrollment reset. LAN and Bluetooth cannot clear a Lite enrollment.

## Remove an offline device's Cloud access

```sh
wendy cloud discover --all --json
wendy cloud device unenroll --device <asset-id-or-name>
```

You can also use:

```sh
wendy device unenroll --cloud-only --device <asset-id-or-name>
```

Cloud-only removal revokes access and deletes the Cloud record without
contacting the device. Its local credentials remain intact. The selector can
also be the device ID shown in an enrollment-conflict error.
A `cloud://` selector, including a saved Cloud default, chooses Cloud-only removal.

## Retry after a failure

For current WendyOS enrollments, Cloud deletes the device first. The device
then checks Cloud's signed reset itself before it erases its keys. If the device
refuses, the command fetches a new signed reset from Cloud and tries twice more.
If the reset still fails, the error prints the command that finishes it. Run it
over the same direct LAN connection:

```sh
wendy device unenroll --device <device-address> --asset-id <asset-id> --yes
```

If that keeps failing, Cloud no longer lists the device, so `--force` (below)
clears its local enrollment. For Lite, reconnect USB and rerun the same command.

Older numeric Cloud enrollments reset locally before Cloud cleanup. They require
an owner or administrator account. If cleanup fails, run the exact Cloud removal
command printed in the error; it retains the old enrollment details needed to
finish. See [device unenrollment](/docs/device/unenrollment) for recovery guidance.

Once a WendyOS device confirms its reset, the command also clears the identity
pins this CLI saved for it, the entries `wendy device unpin` clears. For older
numeric enrollments this happens even when the Cloud cleanup that follows
fails, because the device no longer presents that identity. Until that cleanup
succeeds, Cloud still lists the device, so commands that read Cloud's device
list, such as `wendy cloud discover`, pin it again. If a later connection is
refused, run the `wendy device unpin` command the unenroll output prints.
Cloud-only removal does not contact the device, so it keeps those pins.

## Clear a device that Cloud no longer has

If Cloud has no record of a reachable WendyOS device, `--force` clears only the
device's local enrollment over the LAN so it can enroll again:

```sh
wendy device unenroll --device <device-address> --force
```

It asks for confirmation, or accepts `--yes` (required with `--json`). Cloud
reports "not found" both for a missing device and for one your account cannot
access, so make sure the device is no longer in use. `--force` refuses a device
Cloud still lists, and stops on any other Cloud error. It does not delete a
Cloud record or revoke the device certificate. It needs an updated agent, a
sign-in to the device's organization and a direct LAN connection. To resume an
interrupted Cloud removal, use `--asset-id` instead.

If the connection drops before the device confirms, the command reports the
reset as unconfirmed and keeps the saved device pins. After checking the device
reset, clear them with `wendy device unpin <hostname-or-identity>`.

## Flags

| Flag | Description |
| --- | --- |
| `--device` | Device selector. Cloud-only removal accepts an asset ID, name, or device ID. |
| `--cloud-only` | Remove Cloud access without connecting to the device. |
| `--cloud-grpc` | Cloud endpoint of the saved sign-in to use. |
| `--asset-id` | Check the exact Cloud asset UUID for a current WendyOS enrollment. Not available for Lite or Cloud-only removal. |
| `--check` | Check the Cloud binding and agent reset support without making changes; with `--force`, check the forced local reset instead. Not available for Lite or Cloud-only removal. |
| `--force` | Clear only the local enrollment of a device Cloud has no record of. Does not delete a Cloud record or revoke the certificate. Not available with `--asset-id`, for Lite, or for Cloud-only removal. |
| `--yes` | Confirm removal without a prompt. Required when running non-interactively. |

## Related

- [Enroll a device](./enroll.md)
- [Enroll Wendy Lite](/docs/advanced/cli/lite-enrollment)
