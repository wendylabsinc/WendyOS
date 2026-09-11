# Device enrollment with an OIDC account

```sh
wendy device enroll --name sim
```

Sessions created by OIDC login (`oauthIssuer` is set) automatically obtain
class B enrollment credentials through Cloud. Legacy sessions retain the
Cloud enrollment-token RPC and `--org` override. `wendy cloud enroll-device`
is an alias with the same behavior.

The CLI uses the selected session's tenant; it does not list organizations.
`--org` is rejected for OIDC enrollment. Device names become permanent SPIFFE
identities and may contain paths such as `fleet-a/box-01`. Each segment must
contain 1–64 ASCII letters, digits, dots, underscores or hyphens; empty segments,
`.` and `..` are invalid.

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
4. Cloud returns an asset UUID and the once-only EAB credentials. The CLI
   passes these directly to the agent's v2 `StartACMEProvisioning` RPC.

No credentials file is needed. The login certificate signs the request; PKI
mints the EAB secret. The CLI neither prints nor persists that secret.

The Cloud signature header carries only the operator leaf certificate: Cloud
validates it against PKI's own CA material. Including the ML-DSA intermediates
would exceed the broker's 16 KiB HTTP/2 header limit alongside the bearer token.
The enrollment JWS retains the full chain in the protobuf body.

Cloud does not return an ACME directory URL. For sessions using
`identity.dev.pki.wendy.sh`, the CLI uses:

```text
https://acme.dev.pki.wendy.sh/<session-tenant>/acme/directory
```

Custom deployments can supply `--acme-directory-url <url>`. The directory must
use HTTPS (HTTP is allowed on loopback development servers), and its tenant
must match the selected login.

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
device step fails, Cloud keeps the asset and name reservation. The CLI reports
the asset UUID; restarting the command does not retrieve the original secret
and may encounter that reservation. Automatic recovery across those two
operations requires additional Cloud support. The agent retains its ACME
account key for retries, but Cloud has no credential retrieval RPC.

## Contract sources

`Proto/wendycloud/v2/device_enrollment.proto` is copied unchanged from
Cloud's `cloud-proto/device_enrollment.proto` (commit `c194cf91`). Cloud owns this
public API outside its shared service-protos submodule, but it is vendored
beside the shared v2 contracts and generated from the same include root, so a
re-copy stays a plain `cp`.

PKI's `internal/fabric/enrollment_request.go`,
`docs/reference/api/acme.md` and `docs/reference/fabric-relay-for-issuance.md`
define the enrollment artifact and downstream enrollment protocol.
