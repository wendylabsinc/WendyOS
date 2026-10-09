# Cloud-owned UUID device unenrollment

Implements [WDY-3313 — Cloud v2 device enrollment leaves Jetson unprovisioned after reserving an asset](https://linear.app/wendylabsinc/issue/WDY-3313/cloud-v2-device-enrollment-leaves-jetson-unprovisioned-after-reserving) and [WDY-3534 — Preserve deleted Cloud asset bindings for stateless unenrollment recovery](https://linear.app/wendylabsinc/issue/WDY-3534/preserve-deleted-cloud-asset-bindings-for-stateless-unenrollment).

## One business operation

Cloud owns unenrollment for CLI and console callers. Its signed DeleteAsset path checks device:delete, relays PKI RevokeByPrincipal, then atomically records the tombstone and deletion audit. A relay failure retains the active asset; revocation already completed before an audit/database failure is irreversible and retry finishes deletion. The CLI and Agent do not perform a second ACME revocation. The old prerelease RevokeACMECertificate and CheckACMERevocation RPCs return Unimplemented.

The CLI signs the Cloud request and its exact tenant/device revoke_principal management authority with the same operator leaf. Existing kid/x5c behavior is retained, with a fresh PKI replay identity on each retry. Numeric Cloud v1 follows its original reset-first best-effort path; this does not add a numeric fallback for PKI identities.

## Preconditions and phases

1. Read Agent provisioning through direct verified LAN mTLS. Require CloudUnenrollmentSupported before mutation, exact tenant/operator scope and installed peer principal/fingerprint; reject proxy/plaintext for an active device. Match the enrolled Cloud endpoint.
2. Read GetAsset using organization_id + device_id and **empty asset UUID**. Older UUID-only servers reject before mutation. Validate any --asset-id locally against the authoritative binding. Active returns Asset; authorized deleted returns NotFound plus one typed DeletedAsset. Plain NotFound, status/message text, wrong status, duplicate details, malformed UUID/time or changed binding are never completion proof.
3. If active, invoke Cloud DeleteAsset with expected_device_id and the management request. Re-read retained typed deletion evidence after success. Lost responses mean unconfirmed progress, not rollback.
4. Retrieve issuer-signed OCSP revocation evidence for the exact installed leaf. Fetches dial only checked public IP addresses, do not follow redirects or use environment proxies, and retain standard HTTP/HTTPS OCSP with issuer signatures as authority. This is verification, not revocation. Reject good/unknown, stale/future, forged or mismatched issuer/serial evidence; preserve keys on failure. Issuer signatures support native ML-DSA as well as existing certificate algorithms; no signature verification is skipped.
5. Send existing Agent Unprovision with principal/fingerprint, PKI revocation evidence and exact serialized Cloud deletion binding. The Agent checks same-tenant operator mTLS, current installed certificate, signed revocation and exact Cloud binding before authorizing erasure.

`--check` reads Cloud binding and Agent capability only. It does not revoke, spend EAB, reset, import proof or write a CLI journal. All v2 phases have a three-minute command bound. A false progress flag means unconfirmed, not reversal of a remote mutation.

## Re-entry without CLI workflow state

There is no CLI journal, saved phase, reusable enrollment credential or one-use response token. Historical unenroll/unenroll-v2 journals remain untouched and are not authority.

- Active asset: retry the same Cloud operation; already-revoked principal credentials are a no-op.
- Cloud tombstone with device still provisioned: retrieve fresh PKI evidence and retry guarded erasure.
- Erasure interrupted: before deleting any key, Agent atomically saves and syncs a public, device-signed pending reset authorization in the existing `provisioning.json` under `unenrollment` (`status: pending`, `receipt`). That file survives credential cleanup. Startup verifies the receipt and exact saved enrollment binding and resumes bounded cleanup before loading or serving enrollment. Incomplete recovery fails closed and blocks new enrollment.
- Reset completed but reply lost: after key removal and directory sync, Agent atomically replaces `provisioning.json` with minimal unprovisioned state containing only `unenrollment` (`status: completed`, `receipt`), then syncs the file and directory. No additional persistent recovery files are created. Agent publishes the signed completion receipt only from completed state. Its existing IsProvisioned response carries that public receipt. A fresh CLI validates the old certificate's signature and configured trust roots, PKI evidence at authorization time, and the exact current authorized Cloud tombstone. It reports completion of that **prior principal's operation**, not a newly authenticated current device identity. Plaintext transport/mDNS are not identity proof and carry no mutation authority.

`ReadCompletion` verifies the receipt's signatures and internal binding; it is not a trust-root or current-peer verifier. The CLI separately anchors the historical certificate to its configured roots before trusting remote public evidence. Agent recovery reads only its root-owned 0600 state, originally written after installed-leaf/operator verification; pending evidence is re-bound to that stored identity. Authorization uses Agent wall-clock time and rejects a deletion timestamp more than one second ahead; clock skew can fail closed, requiring clock correction rather than relaxing proof checks.

The receipt contains only public certificate/chain, principal, Cloud endpoint, asset UUID, installed fingerprint, authorization time, PKI-signed status, typed deletion binding and signature. No private key, ACME account key, EAB secret, bearer token or operator key is written to it. Historical development proof files are ignored and preserved. Normal enrollment replaces the existing provisioning state, dropping its prior completion receipt. Historical development files remain ignored and untouched; there is no importer for separate pending/completion files. Downgrading to an Agent that ignores pending recovery is unsafe until the authorized reset has completed.

## Rollout and limits

Flashing is an explicit local reset, not an unenrollment transaction. Valid first-boot provisioning may replace previous pending/completed recovery state. An ordinary restart still verifies and resumes pending reset recovery. Flashing does not implicitly revoke certificates or delete Cloud records; the operator must clean up the old Cloud identity separately. No automatic Cloud-cleanup prompt or workflow is introduced here.

Land [service-protos #96 — Typed deletion evidence through GetAsset](https://github.com/wendylabsinc/service-protos/pull/96), compatible [Cloud #754 — Lifecycle tombstones for safe unenrollment](https://github.com/wendylabsinc/cloud/pull/754) (including Sem's fixes), and an updated Agent before using this consumer. Unreleased development servers/agents are not a compatibility promise. Migration numbers and cumulative landing order must be reconciled before main deployment; source maximum is not a live migration cursor.

Cloud-only unenrollment can finish without a reachable device, but cannot erase an offline device's local credentials. Missing/purged Cloud evidence is unknown and fails closed. Missing/unavailable OCSP evidence preserves local keys even after Cloud completes. A receipt proves the earlier operation, not current physical-device identity or reachability. No live acceptance, installation or deployment is implied by source tests; the earlier production user-run acceptance used a different build.
