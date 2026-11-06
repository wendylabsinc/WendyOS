# `wendy device enroll`

Add a device to your Wendy Cloud organization so you can manage it remotely.
For a new board, first follow its [installation guide](/docs/#2-set-up-a-device).
For Wendy Lite, use the [USB enrollment guide](/docs/advanced/cli/lite-enrollment).

## Enroll a device

Sign in, find your device, then enroll it:

```sh
wendy cloud login
wendy discover
wendy device enroll --device playful-reed.local --name lab-pi-01
```

Replace the address and name. The CLI asks which organization to use when you
have more than one sign-in. Keep the device powered and connected until the
command finishes, then run `wendy cloud discover` to check its Cloud presence.
`wendy cloud enroll-device` is an alias for this command.

The CLI normally renews a nearly expired sign-in automatically. If it asks you
to sign in again, follow the prompt; there is no need to log out first.

> The interactive `wendy device setup` wizard also helps configure Wi-Fi and
> enrollment.

## Choose a name

Use a name that is unique within your organization: up to 63 lowercase letters,
digits, or hyphens, starting with a letter and ending with a letter or digit.
You can change it later with `wendy device rename`.

Without `--name`, the CLI suggests the device's hostname, lowercased and without
`.local`. In a terminal you can accept or replace it. If there is no usable
hostname, enter a name when prompted; scripts must supply `--name`.

The device also receives a permanent identity that Wendy manages automatically.
Renaming it does not change that identity.

## Requirements and troubleshooting

- You need a current agent and permission to enroll in the selected organization.
- A Mac target needs macOS 26 or later for Cloud enrollment. On macOS 15 you can
  still run local apps; update macOS before enrolling.
- If the device was intentionally reset or reflashed, the CLI may ask whether
  to trust its setup connection again. Accept only after confirming that it is
  the device you reset. A failed enrollment preserves the previous saved identity.
- If it is already enrolled, [remove the old enrollment](./unenroll.md) first.

## Flags

| Flag | Description |
| --- | --- |
| `--device` | Address or selector of the device to enroll. Omit it to use normal device selection. |
| `--name` | Device name. Defaults to the hostname when available. |
| `--cloud-grpc` | Select a saved sign-in by its Cloud endpoint. Use this for a self-hosted deployment. |
| `--org` | Organization ID override for legacy enrollments only. Current sign-ins use their selected organization. |
| `--acme-directory-url` | Custom certificate enrollment URL supplied by your administrator. Normally selected automatically. |

For Lite-specific certificate and connection overrides, see the
[advanced Lite enrollment options](/docs/advanced/cli/lite-enrollment#advanced-self-hosted-deployments).

## Remove enrollment later

Use [`wendy device unenroll`](./unenroll.md). For current WendyOS enrollments,
connect over the LAN and use `--check` to verify reset support before removal.
The native Mac agent does not yet support that reset. The
[unenrollment guide](/docs/device/unenrollment) covers offline devices and
interrupted cleanup.
