# Device enrollment with an OIDC account

Direct ACME enrollment is experimental and disabled by default on the agent.
Notifications, mesh routing, and legacy Avahi identity advertisements still
require numeric organization and asset IDs. Use legacy enrollment for those
services. For development, explicitly set `WENDY_EXPERIMENTAL_ACME_ENROLLMENT=1`
in the agent environment before attempting OIDC enrollment. The agent rejects
disabled enrollment before generating keys or spending EAB credentials.

```sh
wendy device enroll --name sim
```

Sessions created by OIDC login (`oauthIssuer` is set) automatically obtain
class B enrollment credentials through Cloud. Legacy sessions retain the
Cloud enrollment-token RPC and `--org` override. `wendy cloud enroll-device`
is an alias with the same behavior.

The CLI uses the selected session's tenant; it does not list organizations.
`--org` is rejected for OIDC enrollment. The CLI generates a UUID for the
permanent SPIFFE device identity. The discovery name is a separate, renameable
DNS label; renaming it does not change the certificate identity.

## Automatic credential handoff

1. Check that the agent is not already provisioned and validate the device
   name and ACME directory before requesting credentials.
2. Call `wendycloud.v2.DeviceEnrollmentService/EnrollDevice` with the OIDC
   bearer token and two separate operator signatures:
   - `x-wendy-request-signature`, scoped to the Cloud method and
     `org/<tenant>/device/<device-id>` resource.
   - `enrollment_request_jws`, with `tenant`, `device_id`, `device_class: B`,
     `iat`, `exp` and a fresh `jti`. This authorization is valid for five minutes;
     that is not an expiration time for the resulting EAB credential.
3. Cloud checks `device:enroll` permission, reserves the asset name and relays
   the enrollment JWS unchanged to PKI's private `RelayEnrollment` RPC.
4. Cloud returns the once-only EAB credentials. The CLI keeps them until the
   agent's v2 `StartACMEProvisioning` RPC redeems them.

No credentials file is needed. The login certificate signs the request; PKI
mints the EAB secret. The CLI never prints that secret; it keeps it
owner-readable under `~/.wendy/pending-enrollments/` only until the agent
redeems it.

The Cloud signature header carries only the operator leaf certificate: Cloud
validates it against PKI's own CA material. Including the ML-DSA intermediates
would exceed the broker's 16 KiB HTTP/2 header limit alongside the bearer token.
The enrollment JWS retains the full chain in the protobuf body.

Cloud does not return an ACME directory URL. The CLI derives one from the
pki-core identity endpoint the session already holds, by replacing its leading
`identity.` label:

```text
https://identity.<rest>/v1/identity/certificate
  -> https://acme.<rest>/<session-tenant>/acme/directory
```

That keeps the derivation inside one PKI deployment and names no environment,
so a self-hosted pki-core derives its own directory exactly as the hosted one
does. A session whose identity endpoint is not an `https://identity.<rest>`
URL derives nothing and must supply `--acme-directory-url <url>`. The directory
must use HTTPS (HTTP is allowed on loopback development servers), and its
tenant must match the selected login.

## Device-side behavior and failures

The agent generates and retains its own private key and ACME account key. It
uses the EAB to register an account and orders a `permanent-identifier`
certificate. Before persisting it, the agent checks the certificate's key and
`spiffe://wendy.sh/tenant/<tenant>/device/<device-id>` identity. Successful
provisioning persists the certificate, chain, principal and directory URL;
it does not persist the EAB secret.

Both Cloud's enrollment service and an agent with `StartACMEProvisioning` are
required. Unsupported deployments produce an explicit error, with no fallback
to legacy enrollment. This command supports class B; class A attestation and
class C EST enrollment are not implemented.

Cloud and device enrollment are separate operations. If Cloud succeeds but the
device step fails, Cloud keeps the name reservation. The CLI reports the device
ID and keeps the credential, so re-running the same command (same session,
same name) skips Cloud and finishes the device step with the same device ID.
The agent retains its ACME account key for those retries. Cloud has no
credential retrieval RPC, so a credential lost before it was kept (a CLI older
than this behavior, or another machine) cannot be recovered: that reservation
holds the name until the device is deleted in Cloud.

## Contract sources

`Proto/wendycloud/v2/device_enrollment.proto` is copied unchanged from
Cloud's `cloud-proto/device_enrollment.proto` (commit `c194cf91`). Cloud owns this
public API outside its shared service-protos submodule, but it is vendored
beside the shared v2 contracts and generated from the same include root, so a
re-copy stays a plain `cp`.

PKI's `internal/fabric/enrollment_request.go`,
`docs/reference/api/acme.md` and `docs/reference/fabric-relay-for-issuance.md`
define the enrollment artifact and downstream enrollment protocol.
