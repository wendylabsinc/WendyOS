# Connect to Wendy Lite through Wendy Cloud

Use Wendy Cloud to reach an enrolled Lite board when you are away from its local
network. The board needs an internet connection and must appear online.

## Find and select your board

```sh
wendy cloud login
wendy cloud discover
```

Use the Cloud selector shown for the board when a command asks for a device.
You can also choose the board in the device picker's Cloud tab. Wendy opens an
encrypted connection to the selected board automatically; you do not need to
configure a tunnel or open an inbound port on its network.

Your account needs permission to connect to the device in its organization.
Use a personal sign-in for Lite connections; service-account sessions do not
support this connection path.

## If the connection fails

1. Check that the board is powered, connected to Wi-Fi, and online in
   `wendy cloud discover`.
2. Confirm that your selected sign-in belongs to its organization with
   `wendy auth status`. Use `wendy auth use` to switch if needed.
3. If authentication has expired, sign in again with `wendy cloud login`.
4. Update the CLI and board firmware if the error reports an unsupported protocol.

Connections have idle and maximum-duration limits. Start the command again if a
session expires. The connection is also limited by certificate expiry.

## Enrollment and removal

[Enroll a board over USB](/docs/advanced/cli/lite-enrollment) before using Cloud access.
USB is also required to clear the board's local enrollment. You can
[remove Cloud access](/docs/advanced/clients/wendy-cli/commands/device/unenroll)
while the board is offline, but that does not erase its local settings.
