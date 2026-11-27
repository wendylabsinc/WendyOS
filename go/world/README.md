# World integration

This tree preserves the World implementation while integrating it with the Wendy
agent, CLI, digital twin and macOS application. The migration baseline is World
commit `3afa7b6e55d735fe6f760ef911b663b96592e5b4`, including the merged feature work,
ESP32 firmware simulation and camera-triggered Go2 response. The C6 localization
changes are also transferred: omnidirectional odometry, stationary updates, exact
terminal orientation and validation of laser transforms. See `INTEGRATION.md`
for the implementation boundaries and current acceptance evidence.

All Go packages use the repository's `github.com/wendylabsinc/wendy` module and
Go toolchain. There is no nested World module. `internal` contains the world and
mission implementation; `production` retains the native identity, per-service
publication, evidence and replication implementation. Its internal packages stay
inside `production` so Go continues enforcing their import boundary.

## CLI and runtime commands

`wendy world` is supported on Linux and macOS and calls `command.Run` in process.
The existing Windows Wendy CLI remains supported; its World command reports the
platform requirement and its MCP server does not advertise World tools. Each invocation owns its context
and output streams. Cancellation reaches network requests; the library does not
exit the caller, register process signals or invoke an older executable.

The following command families are preserved:

| Family | Operations |
| --- | --- |
| `query`, `watch` | Bounded selected replica reads and changes |
| `area`, `replica` | Complete offline snapshots, subscriptions and replica status |
| `frame`, `site` | Geographic projection and signed site distribution |
| `package` | Publication, verified download and resumable cache |
| `evidence` | Fetch, mirror, cache, budgets, pin and release |
| `annotation`, `zone` | Notes and amendments; signed zone catalogs |
| `task` | Catalog, proposal review, signed dispatch, supervision, Stop and history |

Run `wendy world --help` or a concrete operation's `--help` for arguments.
Explicit credentials select the gateway and authority. These preserved command
paths use simulation credentials. Native agent World publication uses its
separate commissioned service configuration. Simulation credentials cannot
authorize physical movement.

`cmd` retains the original service and simulation entry points, including
`worldd`, `world-camera`, `world-patrol`, the mission executors, SensorLink
sources, recording/replay, perception and fusion tools, and scenario launchers.
`cmd/world` is a standalone adapter to the same command library. `worldd` uses
the importable `gateway` implementation. Native source/pair helpers also live in
`cmd`; production readers, publishers, signing and fixture utilities remain in
`production/cmd`.

## Build contexts

Run Go build commands from the WendyOS repository root:

```sh
go build ./go/cmd/wendy
go build ./go/world/cmd/... ./go/world/production/cmd/...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./go/world/cmd/... ./go/world/production/cmd/...
```

Docker recipes use different explicit contexts:

| Recipe | Context |
| --- | --- |
| `go/world/deploy/node.Dockerfile` | WendyOS repository root |
| `go/world/sim/*Dockerfile` | `go/world` |
| `go/world/deploy/patrol-nav/Dockerfile` | `go/world` |
| `go/world/perception/Dockerfile` | `go/world/perception` |
| `deploy/wendy-sim`, `deploy/go2-sim`, cloud-shadow templates | Their generated, allowlisted staging directory |

For example:

```sh
docker build -f go/world/deploy/node.Dockerfile -t wendy-world-node:local .
docker build -f go/world/sim/package.Dockerfile -t wendy-world-package:local go/world
docker build -f go/world/perception/Dockerfile -t wendy-world-perception:local go/world/perception
```

The node recipe uses the root module and includes the command and gateway
packages. Its Dockerfile-specific ignore file limits the context to the required
module metadata, World sources and notices. The final image runs as UID/GID
65532 and contains no client credentials. Third-party notices remain under
`third_party` and are copied into that image.

Staging tools take `--repo /path/to/wendyos/go/world`, which contains their source
and templates. Go resolves the parent module when these tools build `./cmd/...`.
Staging creates a new output directory and keeps generated credentials outside
the image context. Validate the generated `wendy.json` with
`wendy project validate /path/to/staged/project` before using the normal Wendy
app lifecycle. Staging and image building do not deploy an application.

## Migration checks

The initial migration was checked on November 25, 2026:

- The 44 command tests and command vet passed. Additional race checks covered
  invocation cancellation and output handling.
- The built full Wendy CLI passed actual subprocess workflows against local
  authenticated nodes: selected reads over 200 records, resumable evidence
  fetch with fallback, and resumable package download.
- The migrated runtime commands built on macOS and Linux arm64. Their Go tests
  and vet passed after supplying the required simulation source files.
- All ten simulation, navigation and perception COPY source inventories resolved
  within their declared contexts. This checks source availability, not model
  fidelity or successful image builds for every recipe.
- The node image built on Linux arm64. Its `world --help` and `worldd --help`
  commands ran in temporary containers with networking disabled.
- The retained cloud-shadow manifest, an actual generated ordinary node app
  manifest and a freshly compiled reusable Go2 release manifest passed `wendy project validate` without warnings. No device deployment
  was performed for these checks.

## Qualification after source integration

The integrated product has automated and runtime acceptance evidence in
`INTEGRATION.md`. Broader deployment and interactive qualification remain:

- Qualify native World commissioning, service lifecycle and replica/evidence
  operations through the integrated agent; library tests do not prove deployment.
- Exercise the macOS twin, mission review, evidence, annotations, zones and offline
  workflows together with existing Wendy behavior.
- Exercise Chat and native review together with independently selected profiles.
  Signing and execution require operator review; a proposal is not a signed task.
- Extend the completed firmware-camera inspection/return run and five-case fault
  matrix to repeated trials. Retain simulation provenance and original limits.
- Supply external licensed assets and model weights through their existing verified
  paths. They are not copied into this tree as committed images or binaries.
- Complete repeated-run localization qualification before claiming reliable
  inspection and return. One successful integrated run and a passed fault matrix
  are not hardware qualification.

No physical robot motion, hardware readiness, sustained performance or complete
cross-product parity is established by these migration checks.
