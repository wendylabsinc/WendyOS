# Enroll Wendy Lite with pki-core

Build this CLI with Go 1.27 or newer, then use an OIDC operator session for the
Wendy development PKI:

```sh
cd go
go build -o ./wendy ./cmd/wendy
./wendy cloud login --email YOU@YOUR_ORGANIZATION
./wendy cloud enroll-device \
  --device wendy-lite:/dev/cu.usbmodemXXXX \
  --name lite-desk \
  --broker-port 5055
```

This is the same `cloud enroll-device` command used for WendyOS. The selected
device determines the protocol: agent gRPC for WendyOS, or WendyCom over USB for
Lite. Omit `--device` to use the normal picker, or use a USB device ID shown by
`wendy discover`. The hidden `wendy device enroll` alias uses the same dispatch.

The board needs Wi-Fi and the enrollment-capable firmware from
[wendy-lite#57](https://github.com/wendylabsinc/wendy-lite/pull/57) and its dedicated
identity partition. Trust bundles can be provisioned during enrollment, so
the firmware does not need deployment-specific roots. The CLI checks firmware
support before minting a token. Current firmware reports whether PKI, the cloud
connection task and Wi-Fi are enabled. The CLI stops before reserving an asset
if firmware explicitly reports that enrollment is disabled. Older firmware that
does not report this capability remains supported.
Lite enrollment requires a physical USB connection. The CLI rejects LAN,
Bluetooth and Cloud targets before opening a connection or minting a credential.
After reboot, completion checks reuse the selected USB connection.

Signed time defaults to [Sigstore's public RFC 3161 service](https://docs.sigstore.dev/cosign/verifying/timestamps/)
at `https://timestamp.sigstore.dev/api/v1/timestamp`. The CLI bundles Sigstore's
TSA root, discovers device roots from the selected PKI's EST endpoint, and obtains
HTTPS roots from connections verified by the enrolling computer. No time or root
flags are needed with a deployment that supports EST discovery.

Update older Lite firmware before using this default. The verifier must support
Sigstore's P-384 timestamp signer and RFC 3161 signing-certificate issuer details.
Cloudflare's Roughtime service uses a different protocol and is not supported by
this firmware.

For custom device trust, supply `--device-roots device-ca-bundle.pem` and
`--https-roots server-ca-bundle.pem` together. The default TSA root is still
included, and the CLI adds the verified HTTPS root for Sigstore. To use a custom
RFC 3161 service, pass `--time-url https://time.example/v1/time` and provide
`--tsa-roots tsa-ca-bundle.pem` with the device and HTTPS bundles. Obtain custom
bundles from the PKI administrator. Firmware with all three custom bundles pinned
at build time can omit the file flags.

Firmware uses mbedTLS by default. When the board does not advertise ML-DSA
verification, the CLI requests the tenant's P-256 device CA chain and signs that
choice into the Class C enrollment request. PKI binds the token to that authority,
so the device's own P-256 CSR receives a chain it can verify. This requires PKI and
broker versions that support the P-256 device authority; an older PKI returning
ML-DSA roots still stops enrollment before reserving an asset. Explicit device
root files and firmware-pinned roots retain the deployment's configured issuer.
Supplied files must pass the board's algorithm compatibility checks.

Firmware built with `CONFIG_WENDY_PKI_WOLFSSL=y` advertises ML-DSA verification and
retains the normal device CA chain. The flag is disabled by default. A P-256 leaf
beneath an ML-DSA issuer requires this verifier. Both profiles support a TSA signing with
ECDSA P-256 or P-384 and SHA-256. The HTTPS bundle must cover the broker too.

The firmware stores bundles with enrollment configuration and uses them after
reboot. Each bundle supports at most eight CA certificates and 16 KiB. Bundles
plus the signed-time response must fit within 64 KiB. Normal Wi-Fi configuration
updates preserve enrollment. Replacing or erasing the entire configuration removes
its bundles. To change an enrolled board's trust, unenroll it over USB and then
enroll again. This does not introduce remote trust rotation.

The CLI obtains the board's nonce and a signed RFC 3161 timestamp. The board
verifies the TSA chain, signature, nonce, imprint and persisted rollback floor. The CLI then asks
Cloud's `DeviceEnrollmentService.EnrollDevice` for a Class C credential. Both the Cloud
RPC and PKI enrollment artifact are signed with the operator's existing identity.
Cloud reserves the tenant/device asset in PostgreSQL. The single-use token and
public setup data go to the board over USB; its private key is generated
on-device. The CLI reboots the board and waits for certificate installation.

The CSR URL derives from the selected session's `PKIEndpoint`. The default time
service is independent of that deployment.
Self-hosted deployments can pass `--csr-url https://csr.example/v1/TENANT_UUID`
and `--time-url https://time.example/v1/time`. For the development Cloud at
`api.dev.wendy.sh`, the Lite broker defaults to `tunnel.dev.wendy.dev:5055`.
The WendyOS devices hostname does not expose the Lite listener. Other Cloud
endpoints retain their existing hostname selection, with port 5055 by default.
Use `--broker-host` and `--broker-port` for a separately deployed Lite listener.
Use `--cloud-grpc` to select a particular Cloud endpoint/session.

A reserved asset or uploaded configuration is not a successful enrollment.
Failures report the reserved asset ID; rerunning does not silently recover an
expired or existing identity. After enrollment, `wendy cloud discover` shows
presence once the board completes mTLS and the broker's presence checks.

After enrollment, select the online Lite asset through `wendy cloud discover`.
The CLI opens a broker-authorized relay using the selected Cloud session, then
uses the exact operator certificate that signed the relay request to authenticate
directly to the device.
It verifies the device certificate against the enrolled PKI device name and tenant
before sending WendyCom. Keep the ML-DSA login identity for Cloud administration
and enrollment authorization. For the Cloud relay, the CLI uses a separate P-256
Lite operator certificate with the same operator SPIFFE URI. It requests that
certificate from pki-core over the ML-DSA login when it needs one, and stores it
with the login. `WENDY_LITE_OPERATOR_CERT` (leaf and CA chain, PEM) and
`WENDY_LITE_OPERATOR_KEY` override it with a pki-core-issued pair; the CLI
rejects an identity that differs from the selected operator.

The CLI verifies P-256 device chains against the tenant's device authority fetched
over verified HTTPS from the selected PKI. This lookup is needed only when the
existing session trust does not cover the device's issuer. It remains separate
from the Lite operator authority used to authenticate the CLI to the device.

The broker authenticates the device's initial mTLS control connection, requests an
upgrade, and closes that TLS session without closing TCP. It then forwards opaque
bytes between the CLI and the same device socket. The device becomes the TLS
server and requires an operator certificate. The firmware reconnects its control
connection after each handoff so another session can connect while the current
one remains active. Relay payloads contain TLS bytes, with WendyCom framing inside
the encrypted session. Older bare-protobuf relay clients are incompatible.

This requires the coordinated firmware and broker update in
[cloud#753](https://github.com/wendylabsinc/cloud/pull/753), tracked by WDY-3512.

## Remove an old enrollment

A Lite board's PKI device ID is `lite-` followed by its hardware ID, such as
`lite-441bf6804824`. Renaming the board does not release that ID from an existing
cloud asset. Remove the old enrollment before enrolling the same board again.

Use cloud unenrollment when the board is offline or its firmware is corrupt:

```sh
wendy cloud discover --all --json
wendy cloud device unenroll --device <asset-uuid-or-name>
```

The selector also accepts the PKI device ID reported by an enrollment conflict:

```sh
wendy cloud device unenroll --device lite-441bf6804824
```

The equivalent device command is:

```sh
wendy device unenroll --cloud-only --device <asset-uuid-or-name>
```

These commands use your cloud session without connecting to the board. They
revoke its certificates and bootstrap credentials, delete the cloud asset, and
release its device ID. Add `--yes` when running non-interactively. Use
`--cloud-grpc` to select another cloud endpoint/session.

For a USB-connected board, the normal device command checks firmware reset
support, reads its hardware ID and removes the matching cloud enrollment. It then
clears the local enrollment and reboots the board:

```sh
wendy device unenroll --device wendy-lite:/dev/cu.usbmodemXXXX
```

This requires firmware that advertises enrollment reset support. Older firmware
is rejected before any cloud credentials are revoked. Update that firmware over
USB first, or explicitly use `--cloud-only` to remove only the cloud enrollment.
LAN, Bluetooth and Cloud connections cannot clear a Lite board's identity.

The reset removes the device key, certificate, pending issuance and enrollment
configuration while preserving Wi-Fi and app configuration. The firmware checks
the hardware ID and a fresh device challenge before accepting the USB reset.
The CLI verifies that the identity was cleared before requesting a reboot.

If cloud cleanup succeeded but local reset failed, rerun the same USB unenroll
command. It also handles a cloud asset previously removed with `--cloud-only`,
after checking the selected organization's cloud inventory. Authentication or
cloud lookup failures stop the reset. Once unenrollment completes, run
`wendy device enroll` with the same USB device selected. Rebooting alone or
replacing configuration does not clear an issued identity.


Read the firmware console with:

```sh
wendy device logs --device wendy-lite:/dev/cu.usbmodemXXXX
```

This streams buffered and live output without blocking firmware tasks. Ctrl-C
detaches; `--json` reports console chunks with `data`, `stderr`, and `gap` fields.
WendyOS app/service/severity filters, `--tail`, and `--no-follow` are not supported
by the Lite console. USB was tested on the XIAO ESP32-S3; LAN and BLE use the
provider's existing authenticated connection paths.
