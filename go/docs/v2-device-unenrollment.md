# Direct PKI device unenrollment

`wendy --device <LAN-hostname-or-IP> device unenroll --yes` detects a direct
PKI enrollment and performs these ordered steps:

1. Match the Agent's reported principal to its verified mTLS certificate and
   the exact authenticated Cloud asset's tenant and `pki_device_name`.
2. Revoke the **installed leaf certificate** using the existing device ACME
   account (lookup with `onlyReturnExisting`; no EAB, key generation or account
   registration). The Agent durably acknowledges its exact fingerprint.
3. Recheck the Cloud binding and send an operator-signed v2 `DeleteAsset` for
   `asset/<UUID>`.
4. Request a guarded local reset matching both principal and fingerprint,
   then clear only that principal's local identity pins.

`--asset-id <canonical-UUID>` selects the Cloud asset explicitly. It is **not**
its PKI device UUID. Without the flag the CLI requires one unambiguous stored
Cloud binding. `--cloud-grpc` must match the enrolled Cloud host; switching an
operator login or endpoint does not select a different device enrollment.

## Compatibility and authority

Deploy an Agent implementing `RevokeACMECertificate` before using this flow.
An older Agent returns `Unimplemented` before any asset deletion or key reset.
The new Agent refuses legacy v1 reset of direct PKI identities. Direct PKI v2
reset now requires expected principal, expected certificate SHA-256 and a
matching durable revocation acknowledgement; old empty requests fail closed.
Numeric legacy enrollment behavior remains separate and unchanged.

Both new destructive RPC paths require a same-tenant **operator** mTLS peer,
not a device/service certificate or a plaintext connection. Cloud deletion
retains its normal membership, permission and operator-signature checks. A
Cloud-only connection cannot be relied upon after its asset is deleted, so the
CLI requires a directly verified mTLS connection for this workflow.

## Read-only diagnosis

`device unenroll --check --asset-id <UUID> --json` verifies the Cloud/peer binding
and calls `CheckACMERevocation` to check the scoped directory and look up the
existing account with `onlyReturnExisting`. It never sends `revokeCert`,
registers an account, saves revocation progress, deletes an asset or resets keys.
Success means account lookup is ready, **not** that a certificate is revoked.
Errors expose only a fixed phase, HTTP status and allowlisted ACME problem type;
backend details, URLs, nonces and credentials are not printed. An older Agent
fails with `Unimplemented`. A PKI endpoint returning `externalAccountRequired`
for an existing-account-only lookup needs a server-side RFC 8555 correction;
do not supply a fabricated EAB or burn a new credential to bypass it.

## Failures and retries

This is a convergent cleanup workflow, **not a distributed transaction**. A
revocation failure/uncertain response leaves enrollment/account/device keys
untouched and does not delete the asset. Cloud deletion failures leave local
keys untouched, but a previously successful revocation cannot be rolled back.
No numeric-ID fallback or alternate PKI management mutation is attempted.

The Agent stores public principal/fingerprint/serial acknowledgement in
`acme-revocation.json`. The CLI stores a nonsecret, mode-0600 transaction under
its config directory's `unenroll-v2/`. Do not remove these records to force a
retry. A missing asset is accepted on retry only with previously persisted
revocation progress, followed by the Agent's matching acknowledgement. An
unexpected binding or certificate change stops cleanup. A reset response
failure is reported as unconfirmed; inspect supported provisioning status
before reenrolling. Filesystem failures during destructive reset can leave
partial local cleanup and require reconciliation.

This revokes **only the installed certificate**, not every historical
certificate for a principal. Lost account keys, historical issued certificates,
already-reset devices, and device-wide PKI inventory/revocation remain separate
recovery workflows. This change does not deploy a Cloud/PKI server or backfill
metadata. Never replace a failed revocation with deletion of local keys.
