# Apps & Deployments test guide

Local sources on `kb.apps-and-deployments.1`, based on
`b9dcee95facd2f9ab5bee8e95d427abce1c5268a`. No test has been executed.
Only offline compilation, formatting and syntax/source checks were performed.
There were no CLI/Agent runs, browser launches, Cloud/device operations, pushes,
PRs or posts. Commands in this guide are for later authorized preparation/runs.

## Coverage and counts

There are **67 Swift test declarations in 14 command/Console suites**. All have
actual logic and explicit opt-in gates. There are no disabled or empty stubs.
The two excluded scenarios are interactive publish/registration prompts and the
interactive deploy review. Their scripted counterparts remain covered.

| Tier | Declarations | What the bodies exercise |
| --- | ---: | --- |
| Local CLI validation | 8 | Missing input/confirmation, invalid project/required names/services, exact deployment syntax, uncached promotion and rotate identifiers |
| Cloud/CLI/device | 48 | Public operations, actual catalog/registry/history/report reads, independent HTTP responses; includes additional dedicated fleet, service-control, reboot, second-operator and natural-sweep opt-ins |
| Real browser DOM | 7 | Rendered catalog/access/settings/releases, rollout and history, waiting/failed states, removal/deletion/detach and pending device work |
| Isolated protocol/signing | 4 | Real Agent/CLI logic with synthetic in-process peers and deterministic test-only barriers; **not live Cloud/containerd coverage** |

Parameterization expands these to 72 Swift invocations before filtering: 12
local, 49 Cloud/device, 7 browser and 4 isolation. These are source counts, not
runtime discovery or passing results. Four Go top-level isolation cases underlie
the four Swift isolation wrappers and are counted once, not added again. The
bad-data Go case contains six synthetic subcases.

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
scripts. Its error output contains only operation and gRPC code, never auth
structs, keys, tokens or raw signed envelopes. Reads follow every page; a small
page size exercises history/membership pagination.

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
| Removal during pull | **Isolated Agent tier**, blocked blob callback, terminal removal, durable state reload, delayed assignment refusal and no import/start. Real transport interruption/cache behavior is not established by this tier. |
| Replacement overlap | **Isolated Agent tier**, hold B's start, observe C's accepted revision, release B, require the last applied A configuration after both fail. Synthetic runtime peer; no real-containerd timing claim. |
| Bad delivery data | **Isolated Agent tier**, synthetic signature, wrong target, revision zero, manifest, blob and wrap failures; real verifier/pull logic rejects them and the prior working runtime-peer state survives. No real service is attacked or spoofed. |
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

## Prepare fixtures later

The existing harness is unchanged. From the repository root, build only the
in-tree support programs (these commands compile; they do not run tests):

```sh
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -mod=readonly \
  -o Build/e2e-apps-fixture ./go/cmd/e2e-apps-fixture
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -c \
  -o Build/apps-agent-isolation.test ./go/internal/agent/services
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test -c \
  -o Build/apps-cli-isolation.test ./go/internal/cli/commands
```

The isolation runner accepts only four fixed test names in those fixed in-tree
binaries, requires an actual named PASS and rejects skips/no-test output. It
does not accept an environment-supplied helper implementing the action/assertion.
Recompile the binaries from the same source before running this tier.

### Identity and device fixture

No device has been selected or operated by this work. Supply a named approved
Dev Pi, matching CLI/Agent builds, and a dedicated fresh single-session auth
config outside Git with the originating registry key and verified saved
endpoint/tenant/asset/principal pin. Never reuse a Prod board or re-enroll one.
Do not concurrently refresh copied handles. The long sweep needs a dedicated
session whose certificate/access-token lifetime covers its entire budget.

Before any fixture mutation, the Swift helper privately validates its auth
config with plain guards and sanitized errors (no credential dictionaries in
Testing macros). Through the existing harness it then checks public auth
status, the exact online Cloud selector, and the pinned device's read-only
organization, Pi type and ARM64 architecture. Missing, mismatched or expired
identity fails before app creation. Ambient socket, insecure-Cloud and registry
overrides are rejected; the unavailable-registry case supplies a loopback-only
override for that one command.

| Variable | Purpose |
| --- | --- |
| `WENDY_E2E_APPS_DEPLOYMENTS_LOCAL=true` | Local CLI validation tier |
| `WENDY_E2E_APPS_DEPLOYMENTS=true` | Base live Cloud/CLI/device opt-in |
| `WENDY_E2E_APPS_TENANT_UUID` | Expected Dev tenant UUID |
| `WENDY_E2E_APPS_ASSET_UUID` | Expected primary Pi asset UUID |
| `WENDY_E2E_APPS_CLOUD_GRPC` | Explicit Dev host:443 |
| `WENDY_E2E_DEVICE_ADDRESS` | Exact verified primary Pi LAN hostname/IPv4 |
| `WENDY_E2E_CLI_AUTH_CONFIG_PATH` | Dedicated single-session config file |
| `WENDY_E2E_APPS_LAYOUT_A`, `_B`, `_FAIL` | Extracted OCI layout paths on the local CLI host |
| `WENDY_E2E_APPS_FAILURES=true` | Declared failing image / unavailable registry cases |
| `WENDY_E2E_APPS_ACCESS=true` | Grant an existing dedicated member; also supply `WENDY_E2E_APPS_MEMBER_EMAIL` and `_MEMBER_ID` |
| `WENDY_E2E_APPS_CATALOG_FIXTURES=true` | Create/delete an owned never-enrolled compute declaration to exercise missing-key refusal |
| `WENDY_E2E_APPS_BUILDER=true` | Real cached direct run/promotion; requires local Docker/buildx |
| `WENDY_E2E_APPS_ROTATE=true` | Creator key-rotation case |
| `WENDY_E2E_APPS_SECOND_OPERATOR=true` | Unauthorized rotation; also supply `WENDY_E2E_APPS_UNAUTHORIZED_AUTH_CONFIG_PATH` with a distinct fresh same-tenant identity and registered registry key lacking app authority |
| `WENDY_E2E_APPS_FLEET=true` | Private site/tag/multiple-device cases; also supply `WENDY_E2E_APPS_FLEET_PATH` |
| `WENDY_E2E_APPS_DEDICATED_DEVICE=true` | Ordinary Agent/containerd service stop/start on dedicated targets; requires `WENDY_E2E_APPS_SSH_USER`, known SSH host keys and noninteractive sudo for those service controls |
| `WENDY_E2E_APPS_REBOOT=true` | Additional full-reboot opt-in; requires noninteractive `systemctl reboot` on the declared dedicated target |
| `WENDY_E2E_APPS_NATURAL_SWEEP=true` | Long-running hourly sweep case; optional `WENDY_E2E_APPS_SWEEP_TIMEOUT_SECONDS` is 3600–10800, default 7500 |
| `WENDY_E2E_APPS_BROWSER=true` | Real DOM tier; needs the browser dependencies/session below |
| `WENDY_E2E_APPS_ISOLATION=true` | Isolated Agent/CLI boundary tier, independent of live Cloud opt-in |

Use the normal runner with `--no-managed-agent --no-parallel --isolation per-test`
and explicit `--device-address`. Python 3 and curl are required on the local
POSIX CLI host, which must directly reach the app's chosen free port. Commands
have a 180-second bound; ordinary rollout polling uses a 120-second window.
Set the runner's process timeout high enough for the selected tier, especially
the natural sweep; a timeout is a failure, never sweep evidence.

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
before any absence assertion. The retention assertion is exactly the Tagged
releases definition in the Lifecycle policy panel, excluding plan-limit text.
The pre-existing browser is disconnected, not terminated.

## Honest technical limits

No compiled body has been run. Build success establishes type/link correctness,
not environment readiness or passing behavior. Fresh Dev auth, approved devices,
Docker, the dedicated browser and the relevant privileges remain run
prerequisites; supplying an opaque shell helper is never a substitute.

The four isolation cases use synthetic peers around production Agent/CLI code.
They provide finite deterministic barrier/negative checks, not proof of live
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
cleans surviving direct runs on every declared target, compares unrelated
primary inventory, and never resets/re-enrolls a device or prunes shared caches.
Failed cleanup retains an exact app/asset or membership receipt. Do not reuse a
failed sandbox until its pending removal/detach and any retained fleet lease
are reconciled. Evidence excludes private keys, tokens, refresh handles, real
secrets and raw signed envelopes.

The source audit used the control instructions, operator acceptance/reference
cases, Dev findings, WendyOS instructions, harness README/examples, generated
client contracts, CLI/Agent sources and the pinned Console DOM/Lifecycle source.
Offline verification completed on these sources: the Swift test target built
with `swift build --build-tests` using only cached dependencies and
`GIT_ALLOW_PROTOCOL=file`; the adapter built with `GOPROXY=off GOSUMDB=off`;
both Go isolation packages compiled with `go test -c`; browser JavaScript passed
`node --check`; Python parsed with `ast.parse`; Go code was gofmt-formatted and
Swift formatting was scoped to these test files through the repository's make
target. `git diff --check` passed. No resulting test binary was executed.
Go links emitted only the duplicate `-lobjc` warning.

Product behavior,
production workflows and the parked English-to-Swift experiment are unchanged.
Fuzzy IDs, deployment/IaC files and automatic rollout/health gates remain design
ideas rather than asserted requirements.
