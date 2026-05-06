# local-pki-test

End-to-end integration test for the local pki-core enrollment and mTLS flow.
Run this against a local `pkicore serve wendy` instance to verify the full
provisioning and mTLS connection pipeline without a cloud deployment.

## What it does

1. Calls `CreateAssetEnrollmentToken` on pki-core to create a short-lived token
2. Calls `StartProvisioning` on a local wendy-agent to enroll the device
3. Verifies `IsProvisioned` over plaintext gRPC
4. Fetches the CA bundle from pki-core
5. Issues a test-client certificate (for the mTLS client role)
6. Connects to the agent's mTLS port (`agentPort + 1`) with the client cert
7. Verifies `IsProvisioned` over the mTLS connection

A clean run proves that certificate issuance, ML-DSA chain verification, and
the mTLS gRPC server are all wired up correctly end-to-end.

## Prerequisites

1. **pki-core** running with the Wendy frontend:
   ```sh
   pkicore serve all --dev           # starts CA engine + admin API
   pkicore serve wendy --config config.yaml  # exposes CertificateService on :50051
   ```

2. **wendy-agent** reachable over plaintext gRPC (port 50053 by default for this
   tool; the standard plaintext port is 50051).

## Usage

```sh
go run ./cmd/local-pki-test/ \
  --cloud  localhost:50051 \
  --agent  localhost:50053 \
  --api-key dev-secret-change-me \
  --name   my-test-device
```

| Flag | Default | Description |
|------|---------|-------------|
| `--cloud` | `localhost:50051` | pki-core wendy frontend address |
| `--agent` | `localhost:50053` | wendy-agent plaintext gRPC address |
| `--api-key` | `dev-secret-change-me` | Bearer key for `CreateAssetEnrollmentToken` |
| `--name` | `test-device` | Device name embedded in the enrollment token |

## Expected output

```
Connecting to pki-core wendy frontend at localhost:50051 ...
✓ enrollment token: abcdef12... (jti=..., expires=2026-04-25T09:30:00Z)

Connecting to wendy-agent at localhost:50053 ...
✓ provisioning complete
✓ agent is provisioned (org=1 asset=2 cloud=localhost)
✓ CA bundle received (16782 bytes)

Issuing test-client certificate for mTLS connection...
✓ test-client certificate issued

Connecting to agent mTLS port at localhost:50054 ...
✓ mTLS: agent is provisioned (org=1 asset=2 cloud=localhost)

End-to-end enrollment + mTLS flow succeeded.
```
