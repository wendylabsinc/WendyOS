# Pre-Provision Wendy Agent at Imaging Time

**Date:** 2026-04-27
**Branch:** local-pki-test

## Goal

Write a certificate and private key to the config partition during `wendy os install` so the device is enrolled with Wendy Cloud and running mTLS from the very first boot — no post-boot enrollment step required.

## Background

Today's enrollment flow:
1. `wendy os install` flashes the image and writes `wendy-agent` binary + `wendy.conf` (WiFi, device name) to the config partition.
2. Device boots, agent generates a key pair, calls the cloud, gets a certificate, saves it to `/etc/wendy-agent/provisioning.json`.
3. Only after that does the mTLS server start.

The gap: between first boot and enrollment, the device is unprovisioned and the gRPC port is plaintext and unauthenticated.

## Approach

**Approach A — `/config/provisioning.json`**

The CLI pre-generates the key pair, gets an enrollment token, issues a certificate, and writes the full provisioning state as `/config/provisioning.json` on the config partition during imaging. On first boot, `configpartition.Apply()` reads this file, writes it to `/etc/wendy-agent/`, then deletes the source. The `ProvisioningService` loads it naturally since `Apply()` runs before service initialisation.

## Design

### CLI (`go/internal/cli/commands/`)

**New flag on `wendy os install`:**
- `--pre-enroll` (bool) — request pre-enrollment at imaging time

**Interactive prompt:**
- Fires when `--pre-enroll` is not explicitly set, `isInteractiveTerminal()` is true, and at least one auth session exists
- Text: `"Pre-enroll this device with Wendy Cloud? (Y/n)"`

**New function `preEnrollDevice(ctx context.Context, mountPoint string, auth *config.AuthConfig, deviceName string) error`** in `os_provision.go`:
1. Generate a P-256 key pair (`certs.GenerateKeyPair`)
2. Call `certClient.CreateAssetEnrollmentToken` with `OrganizationId` from the auth cert and `Name = deviceName` (if provided) — cloud creates a new asset and returns `orgId`, `assetId`, `enrollmentToken`
3. Generate a CSR with CN `sh/wendy/{orgId}/{assetId}` (`certs.GenerateCSR`)
4. Call `certClient.IssueCertificate` with CSR + token
5. Extract `cloudHost` from `auth.CloudGRPC` (strip port)
6. Write `{mountPoint}/provisioning.json` (mode 0o600) with fields: `enrolled`, `cloudHost`, `orgId`, `assetId`, `keyPem`, `certPem`, `chainPem`

The cloud connection reuses the same mTLS transport already used in `runEnrollDevice` (auth cert + chain).

**Integration in `installLinuxImage`:**
- Calls `pickAuthEntry("")` to resolve the active auth session (errors if there are multiple sessions — user must re-run with `--cloud-grpc`, same behaviour as `wendy device enroll`)
- Pre-enrollment runs after `provisionConfigPartition` and before `ejectDisk`
- Failure is non-fatal (prints warning, install continues) — unless `--pre-enroll` is explicitly set and auth is missing/ambiguous, in which case the command errors before flashing begins
- Success output: `"Pre-enrolling device with Wendy Cloud (org: N, asset: M)..."` then `"Device pre-enrolled. It will be secure from first boot."`

**Key stays in memory only** on the CLI side; it is never written to the local machine's disk.

### Agent (`go/internal/agent/configpartition/`)

**`Apply(logger, configPath string)`** — gains a `configPath` parameter (e.g. `/etc/wendy-agent`). `main.go` already resolves this via `WENDY_CONFIG_PATH` and passes it in.

**New function `applyPreProvisioning(logger, cfgDir, configPath string)`:**
1. Check for `{cfgDir}/provisioning.json` — return silently if absent
2. Read and JSON-unmarshal into a local `preProvisionedState` struct
3. Validate: `enrolled == true`, non-empty `keyPem`, `certPem`, `cloudHost`
4. `os.MkdirAll(configPath, 0o700)`
5. Write `{configPath}/provisioning.json` (mode 0o600)
6. Write PEM files: `device-key.pem` (0o600), `device.pem` (0o644), `ca.pem` (0o644)
7. Write `.provisioned` marker with RFC3339 timestamp
8. Delete `{cfgDir}/provisioning.json`

**Call order in `Apply()`:**
```
applyBinaryUpdate  →  applyWendyConf  →  applyPreProvisioning
```

Since `Apply()` runs before `NewProvisioningService` in `main.go`, the provisioning service's `loadState()` picks up the written files naturally. The device starts fully enrolled; the mTLS server comes up on first boot without any cloud round-trip at runtime.

On subsequent boots, `{cfgDir}/provisioning.json` is absent (deleted on first boot), so `applyPreProvisioning` is a no-op.

## Tests

### CLI — `preEnrollDevice` (table-driven, fake cloud dialer injected via parameter)
- Success path: verifies `provisioning.json` written with correct fields, cert, key
- `CreateAssetEnrollmentToken` RPC error: returns error
- `IssueCertificate` RPC error: returns error
- Cloud returns empty certificate: returns error

### CLI — `installLinuxImage` integration (via `pickAuthEntry`)
- No auth session present: errors before flashing when `--pre-enroll` is set; skips silently in interactive mode when prompt is declined
- Multiple auth sessions with no `--cloud-grpc`: errors with disambiguation message

### Agent — `applyPreProvisioning` (temp dirs for both `cfgDir` and `configPath`)
- Success: source file deleted, all destination files written with correct content and permissions
- Source file absent: no-op, no error
- Malformed JSON: logs warning, deletes source file, no destination files written
- Incomplete JSON (missing `keyPem`): logs warning, deletes source file
- `configPath` not pre-existing: created automatically

## Security Notes

- The private key is on the FAT32 config partition only from imaging time until first boot. This is acceptable for a controlled provisioning workflow.
- After `applyPreProvisioning` runs, the source file is deleted and the key lives only in `/etc/wendy-agent/device-key.pem` (mode 0o600, root-only).
- If the device never boots (e.g. defective hardware), the key on the config partition is stale and the issued certificate can be revoked via the cloud dashboard.
