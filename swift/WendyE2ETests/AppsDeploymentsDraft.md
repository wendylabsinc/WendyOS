# Apps & Deployments test guide

Sources on `kb.apps-and-deployments.1`, based on
`b9dcee95facd2f9ab5bee8e95d427abce1c5268a`. The original handoff was compile-only.
Subsequent authorized regular runs and their failures are recorded separately in
local attempt artifacts. Commands below require the relevant operation and
device authorization; opt-in flags alone do not provide it.

## Coverage and counts

There are **83 Swift test declarations in 15 command/Console suites**. All have
actual logic and explicit opt-in gates. There are no disabled or empty stubs.
The two excluded scenarios are interactive publish/registration prompts and the
interactive deploy review. Their scripted counterparts remain covered.

| Tier | Declarations | What the bodies exercise |
| --- | ---: | --- |
| Local CLI validation | 11 | Missing input/confirmation, invalid project/required names/services, exact deployment syntax and rotate identifiers |
| Cloud/CLI/device | 61 | Public operations, actual catalog/registry/history/report reads, authenticated uncached promotion, scripted first builds and independent HTTP responses; includes additional dedicated fleet, service-control, reboot, second-operator and natural-sweep opt-ins |
| Real browser DOM | 7 | Rendered catalog/access/settings/releases, rollout and history, waiting/failed states, removal/deletion/detach and pending device work |
| Isolated protocol/signing | 4 | Real Agent/CLI logic with synthetic in-process peers and deterministic test-only barriers; **not live Cloud/containerd coverage** |

All eleven former argument variants now have independently named Swift
functions. Each declaration owns one recording/coverage interval; none of the
former inputs or assertions was removed. These are source counts, not runtime
passing results. The four Go top-level isolation cases remain counted once;
the bad-data wrapper retains all six existing subcases.

Eight existing declarations use Cloud-only acceptance fixtures. Their full
device-response counterparts retain terminal and HTTP assertions.

## Cloud-only fixture inputs

`withCloudApp` with its default `verifyTarget: false` is a pure catalog fixture.
It requires the normal saved account reference (`WENDY_E2E_APPS_ACCOUNT` plus
`WENDY_E2E_SAVED_CONFIG_DIR`), the intended Dev tenant and explicit endpoint,
local POSIX CLI, per-test isolation, `--no-parallel` and `--no-managed-agent`.
Publishing also needs its declared OCI layout or ordinary builder inputs.
**It requires no LAN address, asset UUID, device resource file or device pin.**
No device inventory, port probe, HTTP request or runtime cleanup is performed.
Native selection, certificate validity, tenant/endpoint and an actual public
self-permission/profile read are still checked before creating owned resources.

`withCloudApp(verifyTarget: true)` additionally requires the intended asset UUID.
An authenticated public `GetAsset` verifies that exact asset, organization and
compute-device binding before writes. This still requires no LAN address or
LAN pin. The normal deploy/rotate CLI performs recipient-key verification; a
catalog-reserved device name alone is not treated as enrollment proof. Cloud
catalog cleanup is independently verified and makes no runtime-removal claim.

`withApp` remains the runtime fixture: it requires an approved LAN address,
asset, saved binding and actual signed identity, then checks inventory and
independent HTTP responses. Runtime fixture operations reject Cloud-only
instances before device I/O. The run-level audit must still reconcile every
owned Cloud app and any actual owned runtime after a live batch.

The earlier 69-declaration measured baseline remains separate. Two of its cases
read Cloud rollback history and removal/release retention without waiting for a
device result. Seven additional declarations now cover release/configuration
updates, unsigned interrupted-delivery resume, pending key rotation, digest
refusal/retry, target union, manual canary growth, and parameterized scripted
first publication from new/existing projects. These establish their named Cloud
or publication behavior; none replaces the original terminal runtime assertions.
The new-project example derives its app id from a mixed-case directory, builds
the in-tree small scratch HTTP image and verifies an untagged branch@digest
receipt against the actual registry manifest hash.
The remaining added declaration checks a second native operator's rotation
refusal and unchanged Cloud/key state before device acknowledgment. Its strict
permission diagnostic is separate from the original running-device assertion.

Each live case owns a unique app and creates its prerequisites independently.
The core path creates an app and reads its owner/empty catalogs; refuses a
duplicate without changing its name/releases; publishes and verifies release
and encrypted artifact metadata; deploys; compares exact targets, revision,
report/digest/correlation and the real application response; updates release and
configuration; restores earlier content as a new revision; removes, deletes or
detaches and independently observes the corresponding runtime state.

The in-tree `go/cmd/e2e-apps-fixture` adapter uses the existing generated clients,
`cloudsession`, transport/DPoP code and `cloudrequest` signer. It performs public
reads and narrowly scoped fixture operations. Assertions live in Swift. It
accepts no expected responses, assertion JSON, alternate executables or opaque
scripts. Its error output contains only operation, fixed stage and gRPC code, never auth
structs, keys, tokens or raw signed envelopes. Reads follow every page; a small
page size exercises history/membership pagination.

Common deployment setup verifies the publish receipt and `GetArtifact`, including
manifest digest and encrypted descriptors. The publication metadata case also
requires the complete app-filtered `ListArtifacts` result. Listing availability
is a publication assertion, rather than a prerequisite for unrelated rollout
assertions. An unavailable listing remains a failed metadata case.

### Acceptance coverage audit

| Original behavior | Concrete evidence / tier |
| --- | --- |
| Creation and duplicate refusal | GetApp, APP owner/member bindings, empty release/deployment lists, preserved project, original name and published release after refusal |
| Publish vs record reference | Catalog tag/digest/defaults/required names; registry manifest hash, encrypted descriptors and complete app-filtered listing; reference-only release has no artifact |
| Running and install-only | Exact resolved membership, imported/stopped state, later start, independent baked version/attempt/configuration and one runtime instance |
| Configuration precedence | Release defaults → file values → explicit overrides in both service view and HTTP response; malformed file/missing required setting leaves no deployment |
| Offline-to-online catch-up | Dedicated Agent stop/start; desired state exists while waiting, then the same revision applies with a fresh report and HTTP response; browser waiting/running row is separately checked |
| Failed rollout | Declared failing image and mixed A-success/B-failure condition, `--wait` nonzero and its own failed-device summary; timeout 124 is rejected; failed desired content stays separate from the working report/response |
| Fleet selection and waves | Three declared pinned devices; private tag/site union deduplicates A/B and leaves C untouched; old revision membership stays fixed, release-only carry-over and targeting edit are compared; operator-controlled canary/production expansion and explicit move |
| Exact lookup and status | Two actual apps named workshop; distinct deployment UUIDs, releases, configuration, exact asset reports and endpoints |
| Update/config-only | Stable deployment UUID, incremented history, effective settings and changed application response; a new report entry is required when the revision changes |
| Rollback / nonexistent revision | Four retained revisions and rollback linkage to 1; unknown revision 9 refuses without changing B, revision or history |
| Resume after partial delivery | Public config-only UpdateDeployment records pending revision 2 without delivering its statements; normal CLI resume delivers it without adding another revision |
| Resume N-confirm / N+1-sign race | **Isolated CLI tier**, using a forwarding GetDeployment barrier after scripted confirmation and before current-key loading/signPending. It must refuse or sign N's original content. This is separate from the live stale expected_revision refusal test. No live timing proof is claimed. |
| Removal during pull | **Isolated Agent tier**, blocked blob callback, terminal removal, durable state reload and no import/start; pure ordering against the reloaded mark refuses an ordinary earlier revision without signed replay. Full delayed delivery and real transport/cache behavior are not established by this tier. |
| Replacement overlap | **Isolated Agent tier**, hold B's start, observe C's accepted revision, release B, require the last applied A configuration after both fail. Synthetic runtime peer; no real-containerd timing claim. |
| Bad delivery data | **Isolated Agent tier**, synthetic signature, revision-zero, manifest, blob and wrap boundaries, plus unsigned target-validator controls. The acceptance wrapper requires all six named subcases. Focused benign manifest/blob and pure target-validator checks are separate partial evidence; the prior working runtime-peer state survives. No signed cross-target delivery is used. |
| Runtime-down removal | Dedicated containerd stop/start; acceptance is not device completion; runtime/endpoint absence is required after recovery |
| Remove/delete/detach visibility | Removed history, retained reusable release, and a dedicated ctr check of owned image-record removal with an unrelated control preserved; deleted catalog/release/deployment reads and inaccessible artifact; detached device-applied direct-run report plus unchanged HTTP state. Browser independently checks rendered removed history, complete catalog absence and Run directly. |
| Pending deletion vs device removal | Stop the dedicated Agent; catalog deletion finishes while the owned app still responds. Reconnect before requiring runtime absence. Browser must still show the reported device app before removal and omit it only afterward. |
| Interrupted delete / restart | Public signed partial delete accepts A's removal, leaves B pending, then CLI finishes with B detached. A stays absent; B retains its response and direct-run report across a full reboot proven by a changed Linux boot id. |
| Promotion | Real local Docker build and cached direct run A; edit the meaningful version assignment to B without rebuilding; promote must still deliver A. Uncached refusal remains local. |
| Rotation | Remove only the owned runtime/image; rotate; compare new wrapped/sealed key IDs, same release/revision, new applied entry and responding app. A distinct authenticated unauthorized operator must receive authorization refusal; owner wrap IDs, revision, entry and runtime remain unchanged. |
| Retention | Actual organization limit refusal; ordinary override/default roundtrip; opt-in natural hourly sweep must remove an eligible unused B while older in-use A and newest control survive, then A must still respond. No administrator sweep trigger. |

## Sample application and OCI layouts

The complete tiny app is in
[main.py](../../.github/ci-tests/apps-deployments/main.py), with its
[Dockerfile](../../.github/ci-tests/apps-deployments/Dockerfile). It uses Python's
standard HTTP server. `version.txt` is baked into each image; deployment env cannot
change the reported version. The response contract is:

```json
{"app":"owned-app-id","attempt":"unique-attempt","version":"1.0.0","unit":"celsius","fileOnly":"default"}
```

Build these layouts later on a machine with Docker/buildx and ARM64 build
support. The base image is the same immutable multiarch digest already used by
`HTTPAppFixture.swift`. Commands below use a fresh output directory and do not
push to a registry or contact a device. Run from the repository root:

```sh
APPS_LAYOUT_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wendy-apps-layouts.XXXXXX")"
docker buildx build --platform linux/arm64 --provenance=false --sbom=false \
  --build-arg APP_VERSION=1.0.0 \
  --output "type=oci,dest=$APPS_LAYOUT_ROOT/A.tar" \
  .github/ci-tests/apps-deployments
docker buildx build --platform linux/arm64 --provenance=false --sbom=false \
  --build-arg APP_VERSION=2.0.0 \
  --output "type=oci,dest=$APPS_LAYOUT_ROOT/B.tar" \
  .github/ci-tests/apps-deployments
mkdir "$APPS_LAYOUT_ROOT/A" "$APPS_LAYOUT_ROOT/B"
tar -xf "$APPS_LAYOUT_ROOT/A.tar" -C "$APPS_LAYOUT_ROOT/A"
tar -xf "$APPS_LAYOUT_ROOT/B.tar" -C "$APPS_LAYOUT_ROOT/B"
export WENDY_E2E_APPS_LAYOUT_A="$APPS_LAYOUT_ROOT/A"
export WENDY_E2E_APPS_LAYOUT_B="$APPS_LAYOUT_ROOT/B"
```

The failure cases additionally need a layout built with `APP_VERSION=3.0.0`.
That image exits before opening its HTTP listener:

```sh
docker buildx build --platform linux/arm64 --provenance=false --sbom=false \
  --build-arg APP_VERSION=3.0.0 \
  --output "type=oci,dest=$APPS_LAYOUT_ROOT/fail.tar" \
  .github/ci-tests/apps-deployments
mkdir "$APPS_LAYOUT_ROOT/fail"
tar -xf "$APPS_LAYOUT_ROOT/fail.tar" -C "$APPS_LAYOUT_ROOT/fail"
export WENDY_E2E_APPS_LAYOUT_FAIL="$APPS_LAYOUT_ROOT/fail"
```

Retain this source commit, build commands, build logs and layout hashes with the
later run's evidence. No layout has been built here. The published manifest
digest may differ from the input OCI manifest because publish embeds wendy.json.
The fixture parses the actual source receipt:
`Published release <uuid> of <app>: <tag> = sha256:<64 lowercase hex>`.
It accepts that receipt on stdout or readable stderr.

`writeProject` supplies the real source field names: `appId`, `version`,
`language`, `env`, `requiredEnv` and `entitlements`. The host network and HTTP
entitlements expose the allocated port. `E2E_ATTEMPT` exists in env with an empty
default and is required; deploy supplies a unique value. `DISPLAY_UNIT` defaults
to fahrenheit in A and kelvin in B. `FILE_ONLY` has a default and a file override
in the precedence test. All settings are public fixture data.

### Small image for functional runs

The separate [static Go fixture](../../.github/ci-tests/apps-deployments-small/README.md)
builds stripped ARM64 executables in `scratch` for baked versions 1.0.0, 2.0.0
and intentionally failing 3.0.0. It uses the same settings and response contract,
including the declared boot-id condition for mixed-device failure. Its builder
verifies real local HTTP responses, both failure modes, OCI digests and loaded
image equivalence, then removes its verification containers. It uses installed
Go/Docker/Buildx and no fetched Go dependencies or base image.

Select its separately generated A/B/fail paths through the existing layout
inputs for authorized functional runs. Keep the original Python layouts and
large-layer run evidence for the registry upload-timeout regression. Actual
run outcomes, including downstream assertions not reached, belong in separate
attempt reports; fixture verification is not an additional Swift acceptance case.

## Prepare fixtures later

From the repository root, build the
in-tree support programs (these commands compile; they do not run tests):

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -mod=readonly -tags=e2e_apps_fixture \
  -o Build/e2e-apps-fixture ./go/cmd/e2e-apps-fixture
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -c \
  -o Build/apps-agent-isolation.test ./go/internal/agent/services
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -c \
  -o Build/apps-cli-isolation.test ./go/internal/cli/commands
```

The adapter build tag enables only test-driver Site create/delete and owned unenrolled Asset create resource
mappings in the existing Cloud request signer. The generated Site API requires
signed envelopes; the normal CLI's resource map does not yet contain those
methods. The mappings match Cloud's Site and Asset handlers, retain the normal signer,
validity, leaf registration and transport, and are absent from normal product
builds. They authorize no unsigned fallback. Adapter failures expose a fixed
stage name as well as the operation and code, without printing arbitrary errors.

The isolation runner accepts only four fixed test names in those fixed in-tree
binaries, requires an actual named PASS and rejects skips/no-test output. It
does not accept an environment-supplied helper implementing the action/assertion.
Recompile the binaries from the same source before running this tier.

### Identity and device fixture

Select a named, approved Dev device with matching CLI/Agent builds. The primary
and deployment targets need ARM64 layouts. A fleet control can be x86_64 because
no app is assigned to it. Keep actual endpoint, tenant, asset IDs and addresses
in local run inputs, never in the test source.

For an authorized existing account, set `WENDY_E2E_APPS_ACCOUNT` to its saved
context name and `WENDY_E2E_SAVED_CONFIG_DIR` to its normal config directory.
Select that account through `wendy auth use <context>` before the run. The
harness verifies the current context, endpoint and tenant, keeps work files in
an isolated HOME and points only the CLI's config directory at the normal saved
config. It does not copy credentials. Native credential references remain in
their normal store; ordinary CLI refresh uses the original session and lock.
Do not export private keys, registry keys, DPoP keys or access/refresh tokens.
Restore the previous current context after a serialized batch.

`WENDY_E2E_APPS_DEVICES_PATH` names a public resource file with `asset`, `host`,
`architecture` (`arm64`/`aarch64` or `x86_64`/`amd64`) and a `deviceType`
substring for each approved board. The fixture compares both identity and
reported hardware. Desktop Ubuntu may omit `deviceType`: that path requires
`os=ubuntu`, normalized x86_64 architecture and a signed hostname read through
the existing pinned mTLS client. The expected hostname is the optional public
`hostname` field or the authenticated Cloud asset name. No board-model string
is fabricated for Ubuntu. The test-only
adapter's `pin-approved-devices` operation can complete approved LAN bindings in
the normal config after a read-only authenticated GetAsset check. It refuses
any conflicting Cloud, principal or asset constraint; it exports no credentials.
Supply its `--account`, `--cloud`, `--tenant` and `--devices` arguments explicitly.

The original independently supplied single-session fixture mode is retained;
this helper must not manufacture such a fixture from a saved account. Never
reuse a Prod board, enroll a new board or concurrently refresh copied handles.
Long-sweep runs need a normal session valid for the entire budget.

Before any fixture mutation, the Swift helper privately validates its auth
config with plain guards and sanitized errors (no credential dictionaries in
Testing macros). Through the existing harness it then checks public auth
status, the exact online Cloud selector, and the pinned device's read-only
organization and declared device type/architecture (Pi/ARM64 when no resource file is supplied). Missing, mismatched or expired
identity fails before app creation. Ambient socket, insecure-Cloud and registry
overrides are rejected; the unavailable-registry case supplies a loopback-only
override for that one command.

| Variable | Purpose |
| --- | --- |
| `WENDY_E2E_APPS_DEPLOYMENTS_LOCAL=true` | Local CLI validation tier |
| `WENDY_E2E_APPS_DEPLOYMENTS=true` | Base live Cloud/CLI/device opt-in |
| `WENDY_E2E_APPS_TENANT_UUID` | Expected Dev tenant UUID |
| `WENDY_E2E_APPS_ASSET_UUID` | Expected approved primary ARM64 asset UUID |
| `WENDY_E2E_APPS_CLOUD_GRPC` | Explicit Dev host:443 |
| `WENDY_E2E_DEVICE_ADDRESS` | Exact verified primary LAN hostname/IPv4 |
| `WENDY_E2E_APPS_ACCOUNT` | Explicit existing saved Dev account; normal `auth use` must select it |
| `WENDY_E2E_SAVED_CONFIG_DIR` | Normal config directory for saved-account runs; no credential copy |
| `WENDY_E2E_APPS_DEVICES_PATH` | Public approved asset/host/architecture/device-type resource file |
| `WENDY_E2E_CLI_AUTH_CONFIG_PATH` | Independently supplied single-session fixture in legacy fixture mode |
| `WENDY_E2E_APPS_LAYOUT_A`, `_B`, `_FAIL` | Extracted OCI layout paths on the local CLI host |
| `WENDY_E2E_APPS_FAILURES=true` | Declared failing image / unavailable registry cases |
| `WENDY_E2E_APPS_ACCESS=true` | Grant an existing dedicated member; also supply `WENDY_E2E_APPS_MEMBER_EMAIL` and `_MEMBER_ID` |
| `WENDY_E2E_APPS_CATALOG_FIXTURES=true` | Create/delete an owned never-enrolled compute declaration to exercise missing-key refusal |
| `WENDY_E2E_APPS_BUILDER=true` | Real cached direct run/promotion; requires local Docker/buildx |
| `WENDY_E2E_APPS_ROTATE=true` | Creator key-rotation case |
| `WENDY_E2E_APPS_SECOND_OPERATOR=true` | Unauthorized rotation; also supply `WENDY_E2E_APPS_UNAUTHORIZED_ACCOUNT`, a distinct fresh native saved same-tenant identity. The helper validates both public principals, uses standard `registry key show`, and restores the selector. It never copies credentials. |
| `WENDY_E2E_APPS_MEMBER_CONFIG_DIR` | Optional independently signed-in member's normal config directory. Member CLI/API invocations receive this path and their account reference through `env`; owner commands retain `WENDY_E2E_SAVED_CONFIG_DIR`. Equal account aliases in separate directories are allowed only after distinct same-tenant principal verification. Omit this variable to retain the existing single-directory/two-account mode. |
| `WENDY_E2E_APPS_FLEET=true` | Private site/tag/multiple-device cases; also supply `WENDY_E2E_APPS_FLEET_PATH` |
| `WENDY_E2E_APPS_DEDICATED_DEVICE=true` | Ordinary Agent/containerd service stop/start on dedicated targets; requires `WENDY_E2E_APPS_SSH_USER`, known SSH host keys and noninteractive sudo for those service controls |
| `WENDY_E2E_APPS_SYSTEMD_OUTAGE=true` | Alternative Agent-only control through normal authenticated device shell: detect the active supported Agent unit, arm and verify an independent systemd restart at 60 seconds before scheduling its stop, prove mTLS port closure, then verify reconnect, identity, binary hash, service command and unrelated task PIDs. Requires the dedicated-device opt-in, standard mTLS port 50052, Python 3, systemd and ctr. No SSH, override, binary, trust or enrollment change. Other service/reboot/image controls still require their dedicated fixtures. |
| `WENDY_E2E_APPS_REBOOT=true` | Additional full-reboot opt-in; requires noninteractive `systemctl reboot` on the declared dedicated target |
| `WENDY_E2E_APPS_NATURAL_SWEEP=true` | Long-running hourly sweep case; optional `WENDY_E2E_APPS_SWEEP_TIMEOUT_SECONDS` is 3600–10800, default 7500 |
| `WENDY_E2E_APPS_BROWSER=true` | Real DOM tier; needs the browser dependencies/session below |
| `WENDY_E2E_APPS_ISOLATION=true` | Isolated Agent/CLI boundary tier, independent of live Cloud opt-in |
| `WENDY_E2E_APPS_DOCKER_HOST` | Explicit local `unix:///` Docker socket for builder tests; no saved Docker config is copied |
| `WENDY_E2E_APPS_DOCKER_PLUGIN_DIR` | Installed Buildx plugin directory; an empty per-test Docker config references only this public path |

Use the normal runner with `--no-managed-agent --no-parallel --isolation per-test`
and explicit `--device-address`. Python 3 and curl are required on the local
POSIX CLI host, which must directly reach the app's chosen free port. Commands
have a 180-second bound; ordinary rollout polling uses a 120-second window.
Set the runner's process timeout high enough for the selected tier, especially
the natural sweep; a timeout is a failure, never sweep evidence.

The pending rotation authorization example requires the designated member email
and a registered native key, proves no APP-scoped grant, invokes ordinary rotate,
and compares the owner's full history, revision, targets and key identifiers
without waiting for device apply. Its explicit permission-diagnostic assertion
remains strict: an observed missing-deployment-wrap refusal with unchanged state
does not count as a server authorization rejection or pass that assertion.
The existing running-device variant still requires the independent version and
configuration response before and after the unauthorized attempt.

### Dedicated fleet

The fleet file is ordinary resource data, not expected answers or a script:

```json
[
  {"asset":"<A UUID>","host":"<A pinned hostname>"},
  {"asset":"<B UUID>","host":"<B pinned hostname>"},
  {"asset":"<C UUID>","host":"<C pinned hostname>"}
]
```

A must match the primary fixture. All three identities are verified before
site/tag writes. The adapter creates an owned site and unique app-named tag.
It leases only assets it changes; C is never relabeled or assigned. Every
intent is recorded before the update. Lease writes use a same-directory temp
file, fsync, atomic rename and directory fsync. Preparation failure enters
cleanup. Cleanup reads current state, removes only its unique tag, restores a
site only while it still equals the fixture site, preserves unrelated tag/site
edits, records progress atomically and tolerates an already deleted owned site.
The lease keeps both site UUID and original name: Asset updates use names,
while Site APIs and membership reads use UUIDs. Before/after snapshots cover
all three assets and control C inventory even when the main body fails.
A partial cleanup is retryable. Unexpected members in the owned site prevent
its deletion and retain the lease for inspection.

### Real browser

The browser adapter is
`.github/ci-tests/apps-deployments/browser/observe-dom.mjs`, using the pinned
`playwright` dependency from the adjacent package.json. Playwright and its
Chromium runtime are a **runtime preparation dependency**, not installed or
launched here. Install that fixture package normally before opting in.

Launch a separate local Chromium profile with CDP enabled, sign into the
expected Dev organization normally, and set:

- `WENDY_E2E_APPS_BROWSER_CDP` to its loopback CDP URL.
- `WENDY_E2E_APPS_CONSOLE_URL` to the expected HTTPS `*.dev.wendy.sh` origin.

The adapter navigates real app/deployment/device pages in that authenticated
context. It does not inject auth/state, mock routes, call backing RPCs as UI
proof, or use prerecorded DOM. It observes loaded landmarks, exact asset links,
rows, lifecycle-panel definitions and rendered history; polls post-change state
with a 90-second bound; and records screenshots in the test sandbox. It refuses
sign-in redirects, rendered errors, loading states or incomplete catalog pages
before any absence assertion. A loaded real Apps-list preflight runs before
project/resource creation whenever the browser tier is enabled. DOM snapshots
include their observation UTC time for correlation with a bounded outage.
The retention assertion is exactly the Tagged
releases definition in the Lifecycle policy panel, excluding plan-limit text.
Auth redirects produce an actionable origin/path-only sign-in diagnostic. All probe failures use fixed printable text without raw Playwright messages/stacks or OAuth query parameters, preserving JUnit serialization. The pre-existing browser is disconnected, not terminated.

## Honest technical limits

Build success establishes type/link correctness,
not environment readiness or passing behavior. Fresh Dev auth, approved devices,
Docker, the dedicated browser and the relevant privileges remain run
prerequisites; supplying an opaque shell helper is never a substitute.

The four isolation cases use synthetic peers around production Agent/CLI code.
They provide finite deterministic barrier/negative checks. Target validation uses an unsigned own-target control and a value differing only in device identifier; terminal-removal ordering uses the reloaded mark and an ordinary earlier revision without signed replay. These layer checks are not proof of end-to-end cross-target or delayed-delivery handling, live
registry transport corruption, real containerd replacement timing or the Cloud
relay's refusal behavior. The actual resume signing race has its own isolated
case; the separate public stale-update assertion is not counted as that race.
Controlled tampering of real services is not part of this suite.

The mixed two-device `deploy --wait` case uses a benign in-tree application
failure condition, not a service fault or mocked result. It reads public Linux
boot ids over the declared targets' normal SSH connection, supplies B's id as
`E2E_FAIL_ON_BOOT_ID`, and verifies that A runs while B reports failure. The
same image/configuration reaches both devices. Build the layouts from this
source so the declared failure condition is present.

The dedicated image-record case additionally reads `ctr` over ordinary SSH,
using the Agent's actual `default` namespace and `/run/containerd/containerd.sock`.
The expected image name is returned by `services.ReleaseImageName`, not guessed
from terminal output. It proves the owned imported image record exists before
removal, disappears afterward, and preserves a separately deployed responding
control plus every unrelated baseline image record. The SSH fixture needs
noninteractive sudo for that read-only `ctr images list --quiet` command.

Image-record removal does not establish physical content-blob garbage
collection. Physical GCS orphan deletion is likewise not asserted; the natural
sweep case observes actual catalog release deletion and in-use retention, not
bucket contents. These are separate from runtime and image-record assertions.

Cleanup uses the unique owned app id, public release receipts and fleet lease.
It separately reports cleanup errors while preserving the original failure,
cleans surviving direct runs on deployment targets A/B while leaving control C untouched, compares unrelated
primary inventory, and never resets/re-enrolls a device or prunes shared caches.
Failed cleanup retains an exact app/asset or membership receipt. Do not reuse a
failed sandbox until its pending removal/detach and any retained fleet lease
are reconciled. Evidence excludes private keys, tokens, refresh handles, real
secrets and raw signed envelopes.

The source audit used the control instructions, operator acceptance/reference
cases, Dev findings, WendyOS instructions, harness README/examples, generated
client contracts, CLI/Agent sources and the pinned Console DOM/Lifecycle source.
The original offline verification completed on these sources: the Swift test target built
with `swift build --build-tests` using only cached dependencies and
`GIT_ALLOW_PROTOCOL=file`; the adapter built with `GOPROXY=off GOSUMDB=off`;
both Go isolation packages compiled with `go test -c`; browser JavaScript passed
`node --check`; Python parsed with `ast.parse`; Go code was gofmt-formatted and
Swift formatting was scoped to these test files through the repository's make
target. `git diff --check` passed. That original validation executed no test binary; later authorized run outcomes are separate evidence.
Go links emitted only the duplicate `-lobjc` warning.

Product behavior,
production workflows and the parked English-to-Swift experiment are unchanged.
Fuzzy IDs, deployment/IaC files and automatic rollout/health gates remain design
ideas rather than asserted requirements.
