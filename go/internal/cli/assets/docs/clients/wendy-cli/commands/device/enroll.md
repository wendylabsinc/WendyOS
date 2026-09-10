# `wendy device enroll`

Enrolls the connected device with Wendy Cloud (or a local [pki-core](../../../../pki/)) and provisions it with mTLS certificates.

> **Note:** `wendy device enroll` is an advanced command and is not listed in
> `wendy device --help`. It remains fully functional. For most setups, use
> [`wendy device setup`](./setup.md) instead.

## Usage

```sh
wendy device enroll [--name <name>] [--cloud-grpc <endpoint>] [flags]
```

## Description

`wendy device enroll` uses your stored auth session to enroll the connected agent and obtain its certificate. Sign in first with `wendy auth login --email <your-email>` for OIDC enrollment.

Before connecting to the device, enrollment checks your certificate and attempts renewal if it is nearing expiry. If it has already expired, an interactive OIDC session offers to sign in again and then continue enrollment using the same realm and Cloud endpoints. Legacy sessions, non-interactive runs, and `--json` runs stop with login instructions. You do not need to log out first.

› **Certificate identity:** The CSR submitted during provisioning always
› includes the device's authoritative Wendy identity as a URI Subject
› Alternative Name (`urn:wendy:org:‹org›:asset:‹assetID›`). When the
› enrollment token carries a `tenant_uuid` claim, a second URI SAN
› (`spiffe://wendy.sh/tenant/‹uuid›/service/asset-‹assetID›`) is added
› alongside it — cloud requires that SPIFFE principal to sign a relay grant.
› Organizations with no pki tenant get no `tenant_uuid` claim and receive only
› the urn:wendy SAN, exactly as before. The cloud certificate service
› validates these SANs against the enrollment token at issuance time.

The enrolled device is registered in Wendy Cloud under a human-readable **name**. The name can be changed later with `wendy device rename`, so the command resolves it as follows:

1. **`--name <name>`** — always wins when provided.
2. **Hostname default** — when `--name` is omitted and the device is reachable by hostname (e.g. `playful-reed.local`), the name defaults to that hostname with any `.local` suffix stripped (so `playful-reed.local` → `playful-reed`).
3. **Interactive prompt** — in a terminal, when no `--name` is given the command prompts for a name. When a hostname default is available it is shown in brackets and used if you press Enter without typing anything:

   ```
   Device name [playful-reed]:
   ```
4. **Bare IP / no hostname** — when the device is addressed by a bare IP (no resolvable hostname) and `--name` is omitted, there is no default. In a non-interactive environment this fails with:

   ```
   device name is required; pass --name when not running interactively
   ```

   In an interactive terminal it prompts for a name with no default and errors if you leave it blank.

> **Naming an unnamed device:** A device enrolled without a usable name shows up with an empty name in [`wendy cloud discover`](../cloud/discover.md). You can still address it by its numeric asset ID — see [`wendy cloud tunnel --device <id>`](../cloud/tunnel.md).

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | hostname (`.local` stripped) | Human-readable device name. Defaults to the device hostname when omitted; required when the device is reachable only by a bare IP address in a non-interactive environment. |
| `--cloud-grpc` | `""` | Cloud / pki-core gRPC endpoint to use. Overrides session selection; when omitted, the persisted default (set with `wendy auth use`) is used if available, otherwise an interactive picker appears. |

## Examples

Enroll, defaulting the name to the device hostname:

```sh
wendy device enroll --device playful-reed.local
```

Enroll with an explicit name:

```sh
wendy device enroll --device 192.168.1.11 --name lab-pi-01
```

## Related

- [`wendy install` → Linux Desktop](../install.md) — mint a short-lived enrollment token and embed it in the `agent.sh` one-liner so the device self-enrolls on first startup, without needing a USB connection or a running agent.
- [`wendy device setup`](./setup.md) — interactive wizard that provisions, configures WiFi, and enrolls in one flow.
- `wendy cloud enroll-device` — alias for this command, reachable through the cloud tunnel.
- `wendy device unenroll` — reverse enrollment and delete the device from Wendy Cloud.
