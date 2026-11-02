# Enroll Wendy Lite in Wendy Cloud

Enroll your Wendy Lite board to manage it remotely with the Wendy CLI. You need
a USB data cable, current Wendy Lite firmware, a Wi-Fi connection for the board,
and a Wendy Cloud account with permission to enroll devices.

## 1. Connect and sign in

Connect the board to your computer over USB, then run:

```sh
wendy cloud login
wendy discover
```

Find the board's USB selector in discovery. On macOS it looks like
`wendy-lite:/dev/cu.usbmodemXXXX`; use the selector shown for your board.
Enrollment requires USB even if the board is already reachable over Wi-Fi or
Bluetooth. Configure its Wi-Fi before continuing:

```sh
wendy device wifi connect --device wendy-lite:/dev/cu.usbmodemXXXX
```

Choose a network and enter its password when prompted.

## 2. Enroll the board

```sh
wendy device enroll \
  --device wendy-lite:/dev/cu.usbmodemXXXX \
  --name lite-desk
```

Replace the selector and name. If you omit `--device`, choose the USB connection
in the device picker. Select your organization when prompted. The CLI checks the
firmware, sends the enrollment settings, reboots the board, and waits for it to
install its device certificate. Keep USB connected until the command finishes.
The board creates its private key itself.

For hosted Wendy Cloud, the CLI selects the connection and certificate settings
automatically. You do not need to build the CLI or supply certificate files.
If the CLI says the firmware does not support enrollment, update the board
using the [Wendy Lite installation guide](/docs/installation/wendy-lite-esp32).

## 3. Check that it is online

```sh
wendy cloud discover
```

Wait for your board to appear online, then select it for your next command.
Cloud presence can take a little longer than enrollment. The board needs Wi-Fi
and internet access to connect to Wendy Cloud.

Over LAN and Bluetooth, the CLI requires the board's certificate to name the
device UUID the board advertises. Firmware that does not advertise it over
Bluetooth is looked up in the selected Cloud organization by its hardware ID, so
sign in first. Over LAN, update firmware that advertises no device identity.

## If enrollment does not finish

- **The board is already enrolled or has a pending enrollment:** let the pending
  operation finish. To start over, remove its enrollment over USB as shown below.
- **An error reports a reserved asset:** Cloud has created a device record, but
  the board has not finished enrolling. Keep the reported asset ID for cleanup;
  a Cloud record alone does not mean the board is ready.
- **The board is enrolled but offline:** check its power, Wi-Fi and internet
  connection. Read its console for a more specific error:

  ```sh
  wendy device logs --device wendy-lite:/dev/cu.usbmodemXXXX
  ```

  Press Ctrl+C to stop reading. The Lite console does not support the WendyOS
  app filters, `--tail`, or `--no-follow` options.

## Remove or change the enrollment

Sign in to the organization the board is currently enrolled in, connect it over
USB, and run:

```sh
wendy device unenroll --device wendy-lite:/dev/cu.usbmodemXXXX
```

Confirm the removal. The command removes Cloud access, clears the board's local
enrollment, and reboots it. Wi-Fi and app configuration are preserved. You can
then enroll it again, including in another organization.

If Cloud removal succeeds but the USB reset fails, reconnect the board and rerun
the same command. Firmware without reset support, or that does not report its
enrollment identity, must be updated first. If several Cloud records carry the
board's hardware ID, the command lists them and removes none; remove each with
`wendy cloud device unenroll --device <asset-id>`, then run it again.
Bluetooth and LAN connections cannot clear a Lite board's enrollment.

If the board is unavailable, remove its Cloud access without contacting it:

```sh
wendy cloud discover --all --json
wendy cloud device unenroll --device <asset-id-or-name>
```

This leaves the board's local enrollment in place. Before re-enrolling that
board, connect it over USB and run the normal `wendy device unenroll` command.
See [unenrollment options](/docs/advanced/clients/wendy-cli/commands/device/unenroll).

## Advanced: self-hosted deployments

Use settings supplied by your Wendy administrator. `--cloud-grpc` selects the
Cloud endpoint of a saved sign-in. Hosted Wendy Cloud needs no overrides.

| Option | When to use it |
| --- | --- |
| `--broker-host`, `--broker-port` | Your deployment uses a separate Lite relay. |
| `--csr-url` | Your administrator supplies a custom certificate enrollment URL. |
| `--device-roots`, `--https-roots` | Your administrator supplies custom CA certificate bundles. Provide both files together. |
| `--time-url`, `--tsa-roots` | Your administrator supplies a custom signed-time service and its trusted CA bundle. |

The default signed-time service is Sigstore. Keep firmware current so it can
verify that service. Certificate bundles are saved on the board for later boots.
To replace an enrolled board's trust settings, unenroll it over USB and enroll
again with the administrator's settings.
