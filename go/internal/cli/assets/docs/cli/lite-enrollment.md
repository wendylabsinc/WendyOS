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
  --broker-host YOUR_WENDYCOM_BROKER_HOST \
  --broker-port 5055 \
  --device-roots device-ca-bundle.pem \
  --tsa-roots tsa-ca-bundle.pem \
  --https-roots server-ca-bundle.pem
```

This is the same `cloud enroll-device` command used for WendyOS. The selected
device determines the protocol: agent gRPC for WendyOS, or WendyCom over USB for
Lite. Omit `--device` to use the normal picker, or use the USB device ID shown by
`wendy discover`. The hidden `wendy device enroll` alias uses the same dispatch.

The board needs Wi-Fi and the PKI-enabled firmware from
[wendy-lite#49](https://github.com/wendylabsinc/wendy-lite/pull/49) and its dedicated
identity partition. Trust bundles can be provisioned during USB enrollment, so
the firmware does not need deployment-specific roots. The CLI checks firmware
support before minting a token.

All three root flags are required together. Supplying them explicitly authorizes
trust provisioning over the physical USB link. Obtain these files through your
PKI administration process; a certificate chain returned by an unverified endpoint
is not a trusted source. Device, timestamp, and HTTPS services may use different
CA hierarchies. The CLI validates each PEM CA bundle and uses the HTTPS bundle
when requesting signed time.

The firmware stores bundles with enrollment configuration and uses them after
reboot. Each bundle supports at most eight CA certificates and 16 KiB. Bundles
plus the signed-time response must fit within 64 KiB. Normal Wi-Fi configuration
updates preserve enrollment. Replacing or erasing the entire configuration removes
its bundles; changing an enrolled board's trust through the CLI requires operator
recovery. This does not introduce remote trust rotation.

Omit all three flags to retain the existing build-pinned trust flow. A device with
neither provisioned nor embedded roots refuses to connect. Older firmware cannot
accept these flags; upgrade it first.

The CLI obtains the board's nonce, fetches its signed time seed, and asks Cloud's
`DeviceEnrollmentService.EnrollDevice` for a Class C credential. Both the Cloud
RPC and PKI enrollment artifact are signed with the operator's existing identity.
Cloud reserves the tenant/device asset in PostgreSQL. The single-use token and
public setup data go to the board over physical USB; its private key is generated
on-device. The CLI reboots the board and waits for certificate installation.

CSR and signed-time URLs derive from the selected session's `PKIEndpoint`.
Self-hosted deployments can pass `--csr-url https://csr.example/v1/TENANT_UUID`
and `--time-url https://codesign.example/v1/time`. The broker hostname remains
explicit. Use `--cloud-grpc` to select a particular Cloud endpoint/session.

A reserved asset or uploaded configuration is not a successful enrollment.
Failures report the reserved asset ID; rerunning does not silently recover an
expired or existing identity. After enrollment, `wendy cloud discover` shows
presence once the board completes mTLS and the broker's presence checks.

This command does not add a CLI tunnel to Lite through Cloud. That connection
still needs broker-authorized forwarding and end-to-end CLI/device mTLS. The
existing insecure tinycloud client is not used here. See
[cloud#638](https://github.com/wendylabsinc/cloud/pull/638) for broker presence.
