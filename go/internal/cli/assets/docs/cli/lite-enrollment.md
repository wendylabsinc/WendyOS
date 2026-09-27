# Enroll Wendy Lite with pki-core

Build this CLI with Go 1.27 or newer, then use an OIDC operator session for the
Wendy development PKI:

```sh
cd go
go build -o ./wendy ./cmd/wendy
./wendy cloud login --email YOU@YOUR_ORGANIZATION
./wendy cloud enroll-lite \
  --serial /dev/cu.usbmodemXXXX \
  --name lite-desk \
  --broker-host YOUR_WENDYCOM_BROKER_HOST \
  --broker-port 5055
```

The board needs Wi-Fi and the PKI-enabled firmware from
[wendy-lite#49](https://github.com/wendylabsinc/wendy-lite/pull/49), built with
pre-pinned deployment trust bundles and a dedicated identity partition. A normal
Lite release without the enrollment command fails before the CLI mints a token.

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
