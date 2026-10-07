# Direct PKI device unenrollment

`wendy --device <LAN-hostname-or-IP> device unenroll --yes` detects a direct
PKI enrollment and performs these ordered steps:

1. Match the Agent's reported principal to its verified mTLS certificate and
   the exact authenticated Cloud asset's tenant and `pki_device_name`.
2. Revoke the **installed leaf certificate** using the existing device ACME
   account (lookup with `onlyReturnExisting`; no EAB, key generation or account
   registration). The Agent durably acknowledges its exact fingerprint.
3. Recheck the Cloud binding and, if active, send an operator-signed v2
   `DeleteAsset` for `asset/<UUID>` with the expected PKI binding. Confirm its
   exact authorized typed deletion detail from `GetAsset` before reset.
4. Request a guarded local reset matching both principal and fingerprint,
   then clear only that principal's local identity pins.

`--asset-id <canonical-UUID>` selects the Cloud asset explicitly. It is **not**
its PKI device UUID. Without the flag the CLI requires one unambiguous stored
Cloud binding. `--cloud-grpc` must match the enrolled Cloud host; switching an
operator login or endpoint does not select a different device enrollment.

## Compatibility and authority

Deploy Cloud support for tenant/PKI-selected `GetAsset`, typed NotFound deletion
details and durable tombstones, then the updated Agent, before using this flow.
The CLI always reads by tenant/PKI binding with empty UUID, validating any
`--asset-id` locally. Older UUID-only Cloud servers reject the empty UUID before
destructive operations; older Agents return Unimplemented. Previously
hard-deleted rows cannot be backfilled as deletion proof.
The new Agent refuses legacy v1 reset of direct PKI identities. Direct PKI v2
reset now requires expected principal, expected certificate SHA-256 and a
matching durable revocation acknowledgement; old empty requests fail closed.
Numeric Cloud v1 enrollment behavior remains unchanged: it uses the original
reset-first, best-effort Cloud cleanup path without a v2 capability probe or
new command timeout. The three-minute timeout applies only to UUID/v2 cleanup.
The Agent v1 reset refusal above concerns direct PKI/v2 enrollment, not numeric
Cloud v1 devices.

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

The Agent atomically persists only `revokedCertificateSHA256` in existing
`provisioning.json`, syncing file and parent directory before confirmation.
Principal and serial derive from existing state/current certificate; proof must
match that exact leaf fingerprint. It survives restart/failure, is invalidated
by certificate replacement and cleared on reset. Unreleased development
`acme-revocation.json` and nested prototype records are not read or imported;
historical standalone files remain untouched, including during reset.

The CLI has **no unenrollment journal**. Historical `unenroll/` and
`unenroll-v2/` files are left untouched but are not read as authority. Every retry
reconciles the authenticated Agent's exact leaf acknowledgement with Cloud's
**Active / Deleted / Unknown** lifecycle. Deleted evidence retains only asset
UUID, tenant, PKI binding and deletion time for the tenant lifetime; names and
descriptive metadata are removed, names are reusable, and UUIDs are not reused.
Tenant deletion or explicit privacy/admin purge removes evidence. `GetAsset`
returns active Asset or NotFound with typed `DeletedAsset` protobuf detail in
standard gRPC status metadata. Plain NotFound, a missing/malformed detail or
unexpected binding/certificate change always stops cleanup. Never infer deletion
from status code or message text alone. Rollback purges tombstones without
resurrecting assets, losing recovery evidence. Cloud stores no device
certificates or keys and its tombstone does not authorize device reset.

JSON progress booleans indicate confirmed operations, not rollback: a false
field may mean an operation's response was lost. A reset response failure is
reported as unconfirmed; if the Agent already erased its keys, the old identity
can no longer authenticate a retry. Inspect supported provisioning status and
reconcile that completion separately before reenrolling. Filesystem failures
during destructive reset can leave partial local cleanup.

This revokes **only the installed certificate**, not every historical
certificate for a principal. Lost account keys, historical issued certificates,
already-reset devices, and device-wide PKI inventory/revocation remain separate
recovery workflows. This change does not deploy a Cloud/PKI server or backfill
metadata. Never replace a failed revocation with deletion of local keys.
