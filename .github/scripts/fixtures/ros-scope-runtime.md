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
