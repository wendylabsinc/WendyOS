# `wendy device enroll`

Obtains a single-use enrollment credential for the connected device and stages
it on the device, which then enrols itself directly against
[pki-core](../../../../pki/) over **ACME** — or **EST** on constrained hardware
that cannot run an ACME client.

> **Note:** `wendy device enroll` is an advanced command and is not listed in
> `wendy device --help`. It remains fully functional. For most setups, use
> [`wendy device setup`](./setup.md) instead.

## Usage

```sh
wendy device enroll [--name <name>] [--org <id>] [--cloud-grpc <endpoint>]
```

## Configuration

`WENDY_PKI_ACME_ENDPOINT` names pki-core's ACME frontend, e.g.
`https://acme.pki.example`. The tenant path and `/acme/directory` are appended
to it, so only the origin is configuration. It has no default: a host is never
guessed from the cloud endpoint, because guessing one service's address from
another's is what once sent enrollment tokens to the wrong place in cleartext.
With it unset the command stops before requesting a credential rather than
minting one the device could not redeem.

For Wendy's own environments the frontend is `https://acme.dev.pki.wendy.sh` on
dev, and the same host without the `dev.` label on production once that
environment is online.

## Description

Run [`wendy cloud login`](../cloud/login.md) first — the request for the
credential is signed with your operator certificate, and an authenticated
operator is the root of trust at first touch. Wendy ships no factory birth key.

1. The CLI asks for an enrollment credential for the connected device. Cloud
   relays the signed request to pki-core, which mints a credential bound to
   that device and to your tenant, single-use and short-lived. Cloud can
   withhold the request; it cannot issue a certificate.
2. The CLI stages the credential on the connected agent and registers the
   device under a human-readable **name** (see below).
3. The agent generates its own key pair — the private key never leaves the
   device — and redeems the credential against pki-core: an ACME account bound
   by the external-account-binding (EAB) credential, then an order whose only
   identifier is a `permanent-identifier` carrying the device's DeviceID. On
   EST hardware it is a simple enrolment with the same credential.
4. pki-core stamps the device identity
   (`spiffe://wendy.sh/tenant/‹uuid›/device/‹DeviceID›`) into the issued leaf.
   The device asserts no identity of its own: any CN or URI SAN in its CSR is
   replaced server-side, and only the CSR public key is honoured.

No certificate and no private key ever travels through this command.

> **The device's name and its identity are not the same thing.** The **name**
> is unique *within your organization* and is what you address and discover
> the device by; it can be changed later with `wendy device rename`. The
> **DeviceID** is a globally unique UUID minted at enrollment, is fixed at
> mint, and is carried by every certificate the device is ever issued — it
> cannot be changed afterwards. It is deliberately not derived from the name: a
> permanent identity must not be tied to a name that can be reassigned. The
> command prints the DeviceID before enrolling, and it is the only time you see
> it alongside the name you chose.

> **Class B only, today.** The command requests a class B credential: an EAB
> with no attestation challenge. Class A (hardware attestation) and class C
> (EST, for hardware that cannot run an ACME client) are described below
> because they are what the platform supports, but the CLI does not request
> them yet — asking for a credential the device cannot redeem would spend a
> single-use mint for nothing.

> **The credential is single-use.** The agent persists its ACME account key
> before spending the credential, so a re-run after a crash re-registers the
> same account instead of burning a second one. A credential that has already
> been redeemed cannot be reused — run `wendy device enroll` again for a fresh
> one.

The enrolled device is registered in Wendy Cloud under a human-readable **name**, unique within your organization. It is the key `wendy cloud discover`, `wendy cloud tunnel --device` and `--device` lookups resolve, and `wendy device rename` changes it later. The command resolves it as follows:

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

Whichever path supplies it, the name must be a valid DNS label — it starts with a lowercase letter, contains only lowercase letters, digits and hyphens, does not end with a hyphen, and is at most 63 characters. This is the same rule `wendy device rename` enforces, because that command writes one string to both the cloud asset name and the device's mDNS `.local` hostname; a name only one of the two would accept would leave the device unaddressable. A name that fails the rule is rejected before anything is minted:

```
device name "Fleet A Box 01" cannot be used: name may only contain lowercase letters, digits, and hyphens
```

A name already taken by another device in your organization is refused too — compared without regard to case, and checked before any credential is minted, so nothing is spent and you can re-run straight away with a different `--name`.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | hostname (`.local` stripped) | Human-readable device name. Defaults to the device hostname when omitted; required when the device is reachable only by a bare IP address in a non-interactive environment. |
| `--org` | *(ignored)* | Accepted for compatibility and ignored. The organization is the tenant your operator certificate is bound to, so there is nothing left for this flag to select; passing it prints a warning. |
| `--cloud-grpc` | `""` | Cloud gRPC endpoint that relays the credential request. Overrides session selection; when omitted, the persisted default (set with `wendy auth use`) is used if available, otherwise an interactive picker appears. |

## Examples

Enroll, defaulting the name to the device hostname:

```sh
wendy device enroll --device playful-reed.local
```

Enroll with an explicit name:

```sh
wendy device enroll --device 192.168.1.11 --name lab-pi-01
```

## Renewal

Device certificates renew unattended over the same ACME or EST path, with no
operator in the loop. Renewal needs a currently-valid certificate to present:
once a device leaf has expired there is no roll-forward, and the device has to
enroll again.

## Related

- [`wendy install` → Linux Desktop](../install.md) — mint a short-lived enrollment token and embed it in the `agent.sh` one-liner so the device self-enrolls on first startup, without needing a USB connection or a running agent.
- [`wendy device setup`](./setup.md) — interactive wizard that provisions, configures WiFi, and enrolls in one flow.
- `wendy cloud enroll-device` — alias for this command, reachable through the cloud tunnel.
- `wendy device unenroll` — reverse enrollment and delete the device from Wendy Cloud.
- [PKI](../../../../pki/) — the three authorized certificate paths.
