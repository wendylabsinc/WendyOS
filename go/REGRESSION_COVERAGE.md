# Common bug regression coverage

This maps the 12 patterns in the [reviewer skill](../.github/skills/code-review/references/bug-patterns.md)
to executable tests. The historical incidents span August 4 through November 4,
2026. A row means the named mechanisms have regression coverage, not that every
possible occurrence of the pattern is covered. Revisit the relevant row when a
new entry point, persistence format, platform or transport is added.

## Coverage by mechanism

| Pattern | Observable contract and existing coverage | Coverage strengthened in this change |
| --- | --- | --- |
| Replacement transitions | [Container replacement](internal/agent/containerd/replace_integration_test.go) keeps the old task and sockets on preparation failure, suppresses restarts, transfers ownership and reports post-swap failure. [Monitor tests](internal/agent/container/monitor_test.go) cover queued group restarts, explicit stops and overlapping suppression. [NPU teardown](internal/agent/containerd/task_teardown_test.go) retains the owner on observation failure. [Native supervisor tests](../swift/WendyAgentCore/Tests/WendyAgentTests/ContainerServiceSupervisorTests.swift) cover Brewfile waits, failure, overlap and in-flight launch. | The containerd integration suite now runs in CI against a private daemon. An explicitly configured suite fails without root rather than silently skipping. Its runnable image comes from installed static BusyBox, without registry pulls. |
| Restart reconstruction | [Resource restoration](internal/agent/containerd/restart_resources_test.go) checks proxies and serial re-enumeration. [Device-spec tests](internal/agent/containerd/device_spec_test.go) check NPU mounts and cgroup grants after renumbering. [Label tests](internal/agent/containerd/helpers_test.go) model the empty-label loss. | [Metadata reload](internal/agent/containerd/entitlement_persistence_integration_test.go) persists type-only and parameterized grants in real containerd, then reads them through a fresh connection. It also confirms that an empty control label disappears. |
| Background lifetime and teardown | [Audio capture tests](internal/agent/services/data_audio_adapter_test.go) checks that capture outlives the initiating RPC and that an early producer exit is a failure; [ROS stream tests](internal/agent/services/ros2_stream_test.go) exercise oversized messages and teardown. [Output drain tests](internal/agent/containerd/stream_output_test.go) require IO completion before closure. [Recovery tests](internal/agent/containerd/recover_output_test.go) prevent an old output owner ending replacement logs. | Existing lifecycle tests remain in the Linux race suite. The coverage map distinguishes those tests from the containerd integration checks, rather than treating a task exit as proof that output drained. |
| Transport framing and bounds | [Pump tests](internal/agent/services/video_stream_pump_test.go) preserve burst bytes, data returned with errors, cancellation and subscriber shutdown. [Exporter tests](internal/episodeexport/fragments_test.go) cover every two-chunk split, multi-slice pictures, timing and bad index ranges. | The pump now has a retained-buffer regression. [H.264 fuzzing](internal/episodeexport/fragments_fuzz_test.go) varies multiple boundaries and checks exact picture bytes and source-chunk timestamps. [Native BLE tests](internal/shared/ble/central/testdata/notification_bounds_test.m) call the real bridge for GATT length limits, unrelated notification wakeups, fixed deadlines, disconnects and the L2CAP remainder, without a radio. |
| Discovery amplification and obsolete work | [Probe recursion tests](internal/cli/commands/mdns_probe_recursion_test.go) drive the resolver fallback under a probe context. [Discovery stream tests](internal/shared/discovery/stream_test.go) cover address changes, reprobes and batch settling. [DDS pool tests](internal/rtps/pool_test.go) cover sharing, cancellation, cleanup and distinct domains; [SPDP tests](internal/rtps/participant_spdp_test.go) bound replies. | The new Linux job executes [real namespace tests](internal/rtps/netns_linux_test.go) as well as ordinary pool tests. It requires passing results for shared host participants, isolated namespaces and automatic interface selection, so lack of namespace capability cannot silently pass. |
| Identity through fallback and partial completion | [Cloud name tests](internal/cli/commands/device_cloud_name_test.go) preserve tenant pins through selection and certificate preparation; [run fallback tests](internal/cli/commands/device_cloud_name_run_test.go) block secondary unpinned retries. [PKI binding tests](internal/cli/commands/cloud_default_identity_test.go) separate routing IDs from certificate identity. [Certificate refresh](internal/cli/commands/auth_refresh_certs_test.go) protects other contexts and rotated tokens. [Unenroll tests](internal/cli/commands/device_unenroll_legacy_test.go) distinguish device reset from Cloud-only deletion. | Existing tests exercise these distinct paths in the CLI race suite. They are retained rather than replaced with a generic identity helper test. |
| Durable state and recovery | [Episode recovery tests](internal/agent/data/review_fixes_test.go) retain complete manifests, quarantine corruption and count partial data toward quotas. [Atomic writer tests](internal/shared/atomicfile/atomicfile_test.go) exercise file modes and rename failures. [Mac provisioning tests](../swift/WendyAgentCore/Tests/WendyAgentTests/ProvisioningStoreTests.swift) retain enrollment on repair failure and distinguish failed key migration. | [Failed replacement](internal/shared/atomicfile/replacement_test.go) verifies that both public writers preserve the existing state, remove temporary files and permit a successful later retry. Portable state tests now execute natively on Windows as well as Linux. Unix ownership expectations live in a Unix-only file; the Windows counterpart verifies replacement without chown. |
| Time, provenance and loss accounting | [Seal/drain tests](internal/agent/data/seal_drain_test.go) keep late results in their episode, fence pre-roll on receipt and presentation time and reject unavailable boot identity. [Camera adapter tests](internal/agent/services/data_camera_adapter_test.go) check drop accounting and producer rejoin. [Clock tests](internal/agent/data/review_fixes_test.go) distinguish an unbounded system clock from a real conflicting interval. | Existing recording and adapter assertions remain in the race suite. The H.264 fuzz target additionally checks timestamp attribution across transport fragmentation. |
| Failure classification | [Broker tests](internal/cli/sessionbroker/broker_test.go) distinguish canceled uploads from failed transports. [Detached-start tests](internal/cli/commands/deployfastpath_start_test.go) require a successful start before reporting success. [Execution classification](internal/cli/commands/error_class_test.go) checks error categories and preserved causes. | Existing caller-level tests remain in the CLI and broker suites; the new state-replacement test also requires the original failure cause to remain inspectable. |
| Capability evidence and compatibility | [GPU discovery tests](internal/agent/gpudiscovery/discovery_test.go) reject library-only CUDA and avoid treating FastRPC as GPU capability. [Driver OTA tests](internal/cli/commands/drivers_osupdate_test.go) cover kernel matching and unavailable inventory. [Build-host tests](internal/cli/commands/buildhost_test.go) reject automatic chunk delivery on incapable peers. | These existing tests are mapped to the actual consumers, rather than treating a capability display assertion as sufficient. |
| Flash state and changing paths | [Sparse writers](internal/cli/commands/bmap_stale_test.go) clear stale state through both writers. [GPT tests](internal/cli/commands/image_gpt_test.go) preserve partition extents on smaller drives and refuse truncation. [Reconnect tests](internal/cli/commands/os_provision_reconnect_test.go) exercise changed paths, ambiguity, cancellation and absent identity. | [Sparse-range fuzzing](internal/cli/tegraflash/t234/stale_fuzz_test.go) compares clearing ranges against an independent cell model, including overlapping and unsorted input. The oracle requires every written byte to survive. Portable flash tests now run natively on Windows. |
| Representation and selection drift | [Schema parity](internal/shared/appconfig/schema_parity_test.go), [Docker context selection](internal/cli/commands/docker_context_test.go) and [registry names](internal/agent/registry/registry_test.go) check the editor/runtime and producer/consumer boundaries. | Schema parity now rejects schema-only types and keys and duplicate variants, covering both directions of drift. The parity suite also runs natively on Windows. That execution exposed root-relative and drive-relative project paths passing validation. Both fleet and service paths now require `filepath.IsLocal`, with [Windows path tests](internal/shared/appconfig/project_paths_windows_test.go). |

## How CI executes the coverage

[Go Tests](../.github/workflows/go-tests.yml) already runs the Go package suites
with the race detector on Linux. It now also:

- Fuzzes H.264 boundaries and sparse-flash ranges for ten seconds each. Seed cases
  run during ordinary `go test` too.
- Extracts Ubuntu's containerd, runc and static BusyBox into a job-specific
  directory on an ephemeral GitHub-hosted Linux runner. It leaves the runner's
  installed daemon alone. The [runner script](../.github/scripts/test-containerd-regressions.sh)
  creates a private root, state directory and socket, requires every integration
  test to pass, and cleans up its own tasks and daemon. The matrix checks Ubuntu 24.04's original containerd 1.7.12 package and its current
  update, covering both the old layer-parent behavior and the newer daemon.
- Executes DDS namespace isolation with Linux namespace privileges and checks that
  all three selected tests actually passed.
- Runs the Darwin BLE package with cgo and the race detector. The native fixture
  creates no Bluetooth manager; `TestLiveGATT` remains hardware-gated.
- Executes portable state, schema, export and flash tests on Windows, beyond the
  existing cross-compilation check.

The existing required `Test` aggregation includes both new jobs.
[Swift Tests](../.github/workflows/swift-tests.yml) continues to run the complete
WendyAgentCore suite on macOS, including native replacement and provisioning.

For a local portable pass, run from the repository root:

```sh
go test -race -count=1 -timeout 300s \
  ./go/internal/shared/atomicfile ./go/internal/shared/appconfig \
  ./go/internal/shared/discovery ./go/internal/shared/ble/central \
  ./go/internal/agent/container ./go/internal/agent/containerd \
  ./go/internal/agent/data ./go/internal/agent/services \
  ./go/internal/agent/gpudiscovery ./go/internal/agent/registry \
  ./go/internal/episodeexport ./go/internal/rtps \
  ./go/internal/cli/sessionbroker ./go/internal/cli/commands \
  ./go/internal/cli/tegraflash/t234
```

The containerd integration cases skip without `WENDY_CONTAINERD_INTEGRATION`.
Use the dedicated CI job for its private privileged runtime. Do not point this
suite at a production daemon: image and snapshot fixtures are disposable.

## Evidence that still requires hardware or fault injection

The deterministic tests do not establish physical BLE delivery, NPU driver cleanup
under every failure, real USB disconnect/re-enumeration, power-loss durability or
all flash engines. They also do not make a multi-service redeploy transactional.
Those claims need the relevant device campaign and its recorded observations.
A successful cross-build is not a hardware result, and a skipped test is not a pass.
