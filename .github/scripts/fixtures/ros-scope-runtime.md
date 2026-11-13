# ROS scope runtime checks for #2266 and #2300

Run in the existing `containerd-regressions` job after its ordinary integration
binary and extracted runtime tools are available. No new runner is needed.
The preparation command uses ordinary Docker and unprivileged Go compilation;
the three runtime cases require the disposable hosted Linux runner.

```sh
python3 .github/scripts/ros_scope_runtime_test.py
python3 .github/scripts/prepare-ros-scope-runtime.py --output "$RUNNER_TEMP/ros-scopes"
runtime="$RUNNER_TEMP/wendy-runtime/current/root"
common="$RUNNER_TEMP/wendy-runtime/common/root"
runtime_path="$runtime/usr/bin:$runtime/usr/sbin:$runtime/bin:$runtime/sbin:$common/usr/bin:$common/usr/sbin:$common/bin:$common/sbin:$PATH"
for case_name in normal old-loopback fail-open-routes; do
  binary="$RUNNER_TEMP/ros-scopes/$case_name.test"
  if [[ $case_name == normal ]]; then binary="$RUNNER_TEMP/containerd.test"; fi
  sudo env PATH="$runtime_path" GITHUB_ACTIONS="$GITHUB_ACTIONS" RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \
    bash .github/scripts/test-ros-scope-runtime.sh "$binary" "$RUNNER_TEMP/ros-scopes" "$case_name"
done
```

Preserve `ros-scopes/*.log` and `ros-scopes/*.json` with `if: always()`. Do not
upload `ros-image.tar` or the test executables; remove those exact files after
the gate. The native ROS archive is about 728 MiB. Preparation builds a pinned
ROS Humble image, checks fresh DDS payloads between ordinary unprivileged
container processes, exports that exact image, and compiles two controls.
Each hosted case has a 150-second outer deadline and a fresh private containerd
state directory. Direct interruption or uncertain namespace drain retains that
owned directory and writes cleanup-failure evidence instead of deleting beneath
possibly live private mounts. The normal journey observes four six-second DDS windows.

`TestROS2ScopeRuntimeVisibility` uses production `CreateContainerWithProgress`,
including the normal scope resolution and OCI environment injection. Device
and candidate app tasks must share the private host network namespace. Two
external ROS publishers occupy separate network namespaces, connected through
actual veth links. The uplink carries IPv4 and IPv6 default routes; the
peripheral link carries neither. A private sysfs view supplies only the
physical-device marker that veth lacks. The selection policy still reads real
kernel interface flags and route tables. No host interface or physical device
is modified, and no hardware cable classification is claimed.

Fresh nonce-bound, changing DDS payloads must establish these outcomes:

- Network scope sees the device-only publisher and the uplink publisher.
- Peripheral scope sees the device-only publisher and the cable publisher,
  while excluding the uplink publisher.
- Unreadable and oversized IPv6 route observations reduce peripheral scope to
  device-only visibility. Both external publishers remain alive, and both
  external paths were positively observed before the exclusion checks.

An old-loopback control restores the network scope's pre-#2300 CycloneDDS
configuration and must lose the device publisher. A fail-open route control
admits wired interfaces after route-read failure and must leak an excluded
publisher. The wrapper accepts a control failure only at its exact expected
DDS assertion; startup errors, skips and timeouts fail the gate.

The original #2266 contract deliberately accepts an absent IPv6 table on an
IPv4-only kernel. This fixture preserves that supported behavior. Existing
`TestDefaultRouteInterfaces` continues to assert it. The audit's "missing IPv6
route information" requirement is interpreted as failed or incomplete route
observation, not as removing intentional IPv4-only support. No production
policy changes are included.

Here, "peripherals" means discovery on directly attached network interfaces.
Neither original PR grants character-device access, so serial/PTY permission
tests would cover a different feature. Hardware enumeration and actual robot
or sensor traffic remain outside this synthetic topology's claims.

## Audit2022 observer transport

The normal case additionally runs `TestROSObserverHostRuntime` in the same
private runtime, using the same imported ROS image. Preparation copies the
current production lifecycle `observe.py`, `wendy.json` and Go2
`cyclonedds.xml` byte-for-byte into the owned image context; source, staged
bytes and exported image are hash checked before runtime. The executed
observer also reports the three image-file hashes. `ros-humble-nav-msgs` is
the only additional image dependency; no new image build or runner is added.

An actual production-created OCI publisher uses the shipped Go2 Cyclone file
and publishes nonce-bound, advancing Odometry. The unchanged production
Observer class owns subscription, freshness accounting and snapshot; a subclass
only verifies payload ownership and sequence before invoking its callback.
Normal observer delivery, an explicit other-domain isolation control and a
second normal observer must all complete. The second normal's first counter
must exceed the preceding positive observation, so a dead source cannot alone
satisfy exclusion. Each observation is8seconds, the publisher has a60second
ceiling, and the original150second outer namespace deadline bounds the whole
normal suite (including OCI I/O drains). Expected added runtime is roughly
35seconds; this estimate is not measured hosted acceptance.

The original95f6c1c52 changed only `discoveryScope: app` to `host` with explicit
domain0. Laterb8b5222f8 migrated that to `scope: network`; current legacy `app`
maps to device/loopback. The Go2 source itself uses loopback and unicast peers,
so current middleware may support the old configuration too. The fixture runs
that exact config change as a separately classified observation: either
`ROS_OBSERVER_LEGACY_APP_REJECTED` or `ROS_OBSERVER_LEGACY_APP_COMPATIBLE` must
record actual behavior. Compatibility is never counted as a historical
negative-control success. The required domain-isolation control is explicitly
semantic, not falsely attributed to the original fix.

This gate exercises actual OCI/middleware/production observer transport, not
`observe.py`'s HTTP StatusReader/main, simulator VM identity, MuJoCo physics,
motors, dashboard rendering or physical robot middleware. Those claims require
their own full lifecycle evidence. Full normal/control runtime remains pending
until the hosted existing containerd job executes these assertions.
