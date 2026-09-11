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

## Name and identity are two different things

Enrollment produces two identifiers, and only one of them is yours to choose.

The **identity** is minted by the command as a UUID. pki-core stamps it into the device's certificate as `spiffe://wendy.sh/tenant/‹tenant›/device/‹uuid›` and carries it across every renewal for the life of the device. It is irreversible and it is never derived from the name, so relabelling a device cannot rewrite an identity that has already been issued.

The **name** is how you and `wendy` find the device. It is unique within your organization, compared without regard to case, and you can change it later with `wendy device rename`. Because it has to match the device's own hostname, it is a single lowercase DNS label: it starts with `a`–`z`, continues with lowercase letters, digits or `-`, is at most 63 characters, and does not end in `-`. A name that breaks that rule is refused before anything is enrolled.

The command resolves the name as follows:

1. **`--name <name>`** — always wins when provided.
2. **Hostname default** — when `--name` is omitted and the device is reachable by hostname (e.g. `Playful-Reed.local`), the name defaults to that hostname lowercased with any `.local` suffix stripped (so `Playful-Reed.local` → `playful-reed`). A hostname that is not otherwise a DNS label is reported rather than repaired.
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

If Cloud refuses the name — because it breaks the rule above, or because another device in the organization already holds it — it says so and stops. Nothing is minted on that path, so retrying with a different name costs nothing.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | hostname (`.local` stripped) | Human-readable device name. Defaults to the device hostname when omitted; required when the device is reachable only by a bare IP address in a non-interactive environment. |
| `--cloud-grpc` | `""` | Cloud / pki-core gRPC endpoint to use. Overrides session selection; when omitted, the persisted default (set with `wendy auth use`) is used if available, otherwise an interactive picker appears. |
| `--acme-directory-url` | derived | ACME directory URL for a custom pki-core deployment. OIDC sessions only; when omitted it is derived from the session's own pki-core identity endpoint. |

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
