# Remote builds for multi-service projects (WDY-3120)

Extends [2026-08-08-remote-build-host-design.md](./2026-08-08-remote-build-host-design.md).
Read that first: this document only says what changes.

## The problem

`wendy run --build-host` refused any project with a `services` map:

```
✗ build host spark-edeb cannot build multi-service projects; remote builds
  currently support single-service container image projects only
```

The exclusion had no cause in the design. The v1 non-goals list Compose, host-Swift
and Xcode — project shapes that never produce a container image from a build
context — and multi-service is not among them. A service *is* a build context plus
a Dockerfile, which is exactly what the remote path already sends.

The cost of the exclusion fell where the feature pays most. A multi-service CUDA
app for a Jetson is 400+ seconds of local build per service on a Mac, and that is
the cost `--build-host` exists to remove. Worse, it shaped how apps were written:
in `g1-coke-demo` the stage02 services — inference, physical-wrapper, control-ui —
are three separate single-service apps with three manifests, which reads as a
workaround for this limitation rather than a design choice, and costs them shared
top-level `env` and `dependsOn`. Teams were being pushed toward one-app-per-service
to keep remote builds, which is the opposite of what the `services` map is for.

## The shape of the change

`buildServicesParallelCore` already takes the build step as a parameter:

```go
type serviceBuildFn func(ctx context.Context, contextDir, repo, dockerfile string, buildOut, logOut io.Writer) error
```

Three implementations existed (device push, local image store, and the chunk-prepare
path). `buildServicesRemote` is a fourth. Everything else in the multi-service path —
subset selection, dependency order, the progress UI, per-service failure reporting,
`--keep-going`, shared namespaces, readiness and hooks — is untouched, because none
of it knows or cares which machine ran `buildctl`.

That is why `--service X` needs no work: `resolveServiceSubset` runs before either
build path is reached, so narrowing a remote build is the same code that narrows a
local one.

```
runMultiServiceWithAgent
  ├─ opts.buildHost == ""  →  buildServicesParallelWithContent   (unchanged)
  └─ opts.buildHost != ""  →  buildServicesRemote                (new)
                                 └─ buildServicesParallelCore    (shared)
```

## Two names that must agree

The one thing a wrong implementation gets wrong silently. The build host pushes to
`PushTarget.Repository`; the deploy then creates the container from
`localhost:<port>/<app>-<service>:latest`. If those diverge the build succeeds, the
image lands under a name nothing reads, and the container comes up on whatever older
image still holds the expected one.

So `serviceImageRepo(appID, service)` is now the single derivation, used by the local
build, the remote push target and `createService` alike, and `serviceBuildSpec` is a
pure function so the agreement is asserted in a test against the literal reference
format rather than left to inspection.

## One context directory per service

`BuildSpec.app_id` selects nothing on the build host except the stable per-app
directory a context is reassembled into — `BuildImage` clears and re-extracts it
under a per-directory lock, and BuildKit keys its local-source cache on that path.

Sending the group's app id for every service would therefore serialise the whole
group behind one lock *and* hand each build a directory the previous service had
just overwritten, so BuildKit would re-transfer every context on every build. The
spec carries the per-service repo instead: each service gets its own directory, its
own cache and genuine concurrency. Nothing else on the agent reads `app_id`, so this
needs no agent change and works against build hosts already in the field.

## Deliberate limits

**Fleet delivery is refused.** `--device a,b,c` with a service group now errors.
One build delivered to N devices is the single-service path's shape; a group's
lifecycle — dependency order, namespace joins, readiness, hooks — is orchestrated
against one connection. Accepting the flag would deploy the group to the primary
and silently drop the rest, which is the wrong-machine failure this feature's design
refuses everywhere else.

**No push-skip.** A remote build reports an image digest, not the uncompressed layer
identities `QueryLayers` verifies, so it cannot prove what the device holds. The
fingerprint recorder already treats an absent content list as "record nothing", which
leaves any existing verifiable fingerprint intact and makes the next run's skip check
fail closed. A remote multi-service run therefore rebuilds every selected service —
correct, and no worse than the local multi-service path, whose push-skip is also
inactive today. Restoring it needs the build host to report layer identities, which
is its own change.

**Capabilities are checked once per group**, not once per service: builder role,
BuildKit presence, buildkit root space, platform support and chunk-delivery support
are properties of the host, and per-service checks would print the emulation and
chunk-delivery notices once per service.

## What `rejectUnsupportedBuildHostProject` still refuses

Compose, provider targets, Xcode and host-toolchain Swift — the shapes that do not
produce a container image from a build context. Its message no longer says
"single-service", which had become the instruction to split an app into three.
