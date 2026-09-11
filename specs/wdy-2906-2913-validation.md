# WDY-2906–WDY-2913 implementation and validation

Implemented on 2026-09-07 from WendyOS `de38678cc0520e85f3831584620503f19b67a881`,
WendyOS-Builder `61fdb278944280b8d1ea892e1229336d27df2208`, and templates
`99343ea7a4e7590e03b0fab592538c7cc268c110`. Work is isolated on
`ed/wdy-2906-2913` in each repository. Original checkouts were preserved.

On 2026-09-10 the stack was rebased onto WendyOS main `54becfbcc` (after the
Dragonwing flash work in #1921 rewrote GPU vendor detection, now reconciled into
`gpudiscovery`), the new `container_storage`/`gpu_capabilities` fields were
renumbered clear of the NPU fields in #1936, and Qualcomm Dragonwing support
(`qnn` compute backend, Adreno render-node grant in the `gpu` entitlement) was
added per review. Go/Swift builds and the test suites listed below passed again.

On 2026-09-11 the stack was rebased onto WendyOS main `9a474f749`, past the NPU
fields that #1936 merged; the field renumbering folded into the single protocol
commit. Per review, `gpu_capabilities` became a repeated per-GPU list (each entry
carries `vendor`, `path`, and `compute_backends`, so a host with an AMD and an
NVIDIA card reports rocm and cuda against the right GPU; no entries on a device
that reports a GPU identifies an older agent), and the Mac agent feature was
renamed from `native-process-v1` to `native-process` to match the unsuffixed
feature vocabulary. The docs-coverage review's findings were addressed: the device
info reference documents `containerStorage`, and the init reference describes
`mojo` on `darwin`. Go/Swift builds and the test suites listed below passed again.

## Reviewable changes

| PR | Commit | Change |
|---|---|---|
| [1906](https://github.com/wendylabsinc/WendyOS/pull/1906) | `e3b786a7d` | WDY-2907: effective service environment, CLI precedence, fingerprints/watch, extended ComposeEnv fixture |
| [1907](https://github.com/wendylabsinc/WendyOS/pull/1907) | `385b621df` | WDY-2906/2913: actionable network warnings, explicit none/bridge selection, empty isolation cache state, persisted CNI cleanup |
| [1908](https://github.com/wendylabsinc/WendyOS/pull/1908) | `e29230eea` | Compatible v1/v2 storage and GPU capability fields with Go/Swift bindings regenerated together |
| [1909](https://github.com/wendylabsinc/WendyOS/pull/1909) | `42eaa94b2`, `95415bebe`, `f971cafc9` | WDY-2908/2909: container-storage filesystem, shared GPU discovery and compute capabilities, CLI/MCP and CUDA build hint |
| [1910](https://github.com/wendylabsinc/WendyOS/pull/1910) | `f8952b09d` | WDY-2910: continued readiness, service state inspection, lifecycle cancellation, deferred hooks/browser actions, unknown-key warnings |
| [1911](https://github.com/wendylabsinc/WendyOS/pull/1911) | `b25cd8189`, `5bdc857cb` | WDY-2911: native command/cwd, file sync, capability negotiation, environment and resolved launch persistence, PID birth identity |
| [1912](https://github.com/wendylabsinc/WendyOS/pull/1912) | `99544c2d4` | WDY-2912: quiet log heartbeats, MCP filtering, Go ACK timeout 20 seconds |
| [1905](https://github.com/wendylabsinc/WendyOS/pull/1905) | `ce139bf00`, `5952f9a76`, `c8cc6e996`, `e63dbcf8d`, `83311db7a`, `97ace30a0`, `01a30e291`, `d5874783b`, `b7e0c627e` | Darwin template selection, cross-repository scaffold acceptance, native app-ID guard, portable process fixtures, validation evidence |
| [templates 103](https://github.com/wendylabsinc/templates/pull/103) | `73f275e`, `421087d`, `f1450b1` | CUDA migration, generic Ollama vendor path, Mojo/MAX Mac chat, catalog, supervisor tests and findings |

The agent PRs are stacked in the order above. Each diff is below the security
review's 140,000-byte limit. The original aggregate PR exceeded that limit, so
no partial security review was accepted. Regenerating with the pinned Swift
plugin removed unrelated changes; separating protocol from consumers keeps
both Go and Swift bindings together in one reviewable protocol change.

Before splitting, GitHub's Go tests, Swift tests, Mac build, local macOS/Ubuntu
E2E, format/vet/lint, vulnerability scan, CodeQL and docs checks passed. After
final generation, affected Go tests and all 359 Swift tests passed again.
The component PRs run their own build/test checks; their live GitHub status is
authoritative. Security, API and docs review workflows are scoped to PRs targeting
`main`. Those reviews must run for each component as its predecessor lands and it
is retargeted to `main`; retargeting alone is not a configured security trigger,
so a subsequent synchronize/reopen event is required before merging. Stacked
base branches do not waive any main-branch review requirement.

Existing `.mojo` rendering, single-service entitlement fingerprints, startup
ordering, cloud client keepalive intervals, and persisted CNI cleanup were
preserved. The historical 256 MiB group memory observation is not a current limit.

## Automated validation

All commands below passed after the final applicable changes:

- `TMPDIR=/private/tmp go test ./go/internal/cli/commands ./go/internal/shared/appconfig`
- `go test ./go/internal/agent/gpudiscovery ./go/internal/agent/services ./go/internal/agent/hardware ./go/internal/cli/commands ./go/internal/cli/mcp`
- `WENDY_TEMPLATES_CHECKOUT=<templates checkout> TMPDIR=/private/tmp go test ./go/internal/cli/commands -run 'TestMacLLMTemplateScaffold|TestOfferPortBusyRetry|TestNativeCommand'`
- Linux arm64 Go 1.26.6: `go test -race ./go/internal/agent/containerd ./go/internal/agent/services ./go/internal/cli/commands ./go/internal/cli/mcp ./go/internal/agent/gpudiscovery ./go/internal/shared/appconfig`
- Repository-wide `go build ./go/...` and `go vet ./go/...` on macOS arm64 and Linux arm64.
- Windows amd64 CLI cross-build and CLI test-binary compilation passed.
- `make -C swift test`: **359 tests in 59 suites passed**. Required `make -C swift format` completed before Swift commits.
- Templates: `python3 -m pytest -q --ignore=common/mojo/wendynet/test_ws_echo.py`: **35 passed**. The excluded fixture needs a separately running Mojo echo server and is unchanged.
- `docker build --check --build-arg WENDY_GPU_VENDOR=broadcom --build-arg WENDY_JETPACK_MAJOR=0 python/llm/ollama`: passed without warnings.

Linux tests ran in `golang:1.26.6-bookworm` with libusb/asound development packages
and `--tmpfs /tmp:exec`. Docker's root overlay correctly trips the pre-existing
agent update safety guard, so update test binaries require a non-overlay temp
filesystem. A pre-existing process-holder fixture used PID 1, which can be a
protected container ancestor; it now uses unrelated synthetic PIDs. Go timers
are exercised with `testing/synctest`; mounts, GPU sysfs, and process identities
use deterministic injected fixtures.

Three template checks also failed on the untouched baseline: the CUDA-12 shim
check mistakenly included the Mojo/MAX Dockerfile, and the Markdown checker
interpreted code as a link. Their scopes were corrected; all 35 self-contained
checks now pass.

## Apple Silicon acceptance

Target: **Apple M4 Max, 16 CPU cores, 48 GiB**, macOS **26.6.2 (25G83)**.
CLI version **dev**; agent **0000.00.00-000000-dev**. These are development
artifacts, not shipped releases. MAX **26.5.0/Python 3.14.7**, Open WebUI
**0.9.5/Python 3.11.16**, uv **0.12.10**. Builds used Go **1.26.6** and Apple
Swift **6.2**. Agent startup/restart/stop used the repository Makefile workflow;
**WendyAgentMac was stopped after verification**.

| Check | Verified outcome |
|---|---|
| Metadata | `hasGpu: true`, vendor `apple`, `gpuCapabilities: [{vendor: apple, computeBackends: [metal]}]` |
| Native environment/cwd | CLI repeat uses last value, empty override retained, cwd is the synced `data` subdirectory, agent app identity applied |
| Slow startup | Initial deadline 1 s; “still starting”; ready at 11 s; exactly one host hook |
| Mac chat | MAX binds `127.0.0.1:11435`; WebUI serves port 8080; browser-chat API lists the model and returns “Hello!” via MAX |
| Final fresh runtime | Uninterrupted package installation, model download, compile, and browser readiness in approximately 465.5 s; MAX healthy after 109.6 s; default browser hook opened exactly once after readiness |
| CLI scaffold | Final CLI fetched the published branch and scaffolded `mac-llm` with `--target darwin --language mojo`; generated configuration and launcher match the template |
| Cold model | Download 28.6 s, compile 29.3 s, MAX healthy in 83.8 s |
| Warm redeploy | Environments reused, compile 0.6 s, MAX healthy in 9.1 s; prior launcher stopped |
| Persistence | Installation stamps, secret and runtime marker retained byte-for-byte and by inode across redeploy and agent restart; WebUI account remains usable |
| Agent restart | App recovered automatically with a new PID and persisted birth identity |
| Child failure | Terminating the confirmed WebUI child stopped MAX; native restart policy recovered the app |
| Final stop | Both 8080 and 11435 listeners closed; development agent stopped |
| Quiet logs | One Mac loopback subscription ran 625 s, received the app's message after 605 s silence, rendered one JSON line and no heartbeats |

The first package-install run exposed a MAX 26.5 CLI mismatch: `--host` is not
supported. The delivered launcher uses the installed version's
`MAX_SERVE_HOST=127.0.0.1` setting. After the corrected model-cold and warm runs,
the delivered template completed an uninterrupted deployment under a new app ID
with an initially absent runtime directory. Both environments, model/cache, and
WebUI data were created afresh. It reached browser readiness within the 600 s
budget, opened the browser once, and answered through WebUI. This is fresh-runtime
evidence on the same Mac, which already had uv installed.

The raw run outputs behind the table above (browser chat result, readiness log,
deploy/restart/child-failure/cleanup results, quiet-subscription result) were
reviewed for this record and are not kept in the repository. Development
artifact SHA-256 values:

- CLI: `e7090a14c8b746652ba5edc84ef3f78a4b97788d7fd91e7d70b93e77828e256a`
- Final CLI used for scaffold and final shutdown: `bf754a3acab84baa68ffaea460cc733574aca0a252fe2e565d97300d279e6438`
- Mac agent archive: `3810dbc9971d2c986cabbcfced51a18399202cd0e9674ac025d932383978b4b2`

## Required before release / issue closure

Hardware discovery found no live local Orin Nano or Raspberry Pi. Cloud discovery
listed Thors `joannis-agx-thor` and `Wendy Studio Thor Gamma`; target selection was
requested and remains unanswered. No shared cloud device was modified.

Still required:

- Orin/Thor/Pi networking acceptance: omitted warning, host LAN access, bridge
  outbound DNS/download, explicit none quiet.
- Both-service runtime CLI override acceptance on Linux, including watch recreation.
- Thor `df` versus CLI/MCP storage identity and capacity; root-only pressure advice.
- Physical Pi Broadcom/no-CUDA and NVIDIA/AMD backend acceptance.
- Full group redeploy removing isolation/changing entitlements, every namespace,
  and CNI cleanup without agent restart or app removal.
- One ten-minute silent subscription over **real direct WiFi and cloud**. Local
  loopback and simulated clocks do not substitute for those routes.

No stable agent/CLI release or issue closure has been performed. After hardware
acceptance, publish the reviewed agent/CLI release, then update
`recipes-core/wendyos-agent/wendyos-agent_1.0.bb` in WendyOS-Builder:
`WENDYOS_AGENT_VERSION`, `WENDYOS_AGENT_SHA256_arm64`, and
`WENDYOS_AGENT_SHA256_amd64`, using the actual published tarballs. CI already
resolves the latest stable release. The fallback is deliberately still
`2026.07.03-194041`; no fabricated version or checksum was introduced.
