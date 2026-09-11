Controls whether a device will run image builds submitted by other people with
[`wendy run --build-host`](../run.md#remote-build-host).

For a complete example that sends one build to several devices, follow [Build Once, Deploy to Several Devices](/docs/guides/fleet-deployment).

```sh
wendy device build-host enable  --device spark-office
wendy device build-host status  --device spark-office
wendy device build-host disable --device spark-office
```

## The role is off by default

A remote build executes the caller's build instructions on the host. Enabling
the builder role allows users in your organisation to run builds there. Use a
host intended for shared builds.

The role requires an explicit `enable` command. Installing the agent,
provisioning the device, or installing BuildKit does not enable it.

Enabling takes effect on the next build. There is no agent restart.

## Who may change it

`enable`, `disable`, and build submission require a **user** certificate.
Device certificates are rejected, including on hosts with the builder role
enabled. The unauthenticated port the agent serves before provisioning does not
accept builds.

The one caller without a certificate is an on-device container holding the
**admin entitlement**, which reaches the agent over a local unix socket. There
the socket's own permissions are the credential, and that entitlement already
carries full agent authority.

Cross-organisation callers are rejected before any of this is reached, by the
agent's mTLS organisation check.

## Source contexts and concurrent builds

The host stores source files under `/var/lib/wendy/buildctx/<app>` between
builds so BuildKit can reuse its local-source cache. The files are cleared and
rewritten at the start of each build of that app, but are not deleted after
the build finishes.

Builds of the same app are serialised on a host. Builds of different apps can
run concurrently. This prevents one build from replacing source files that
another build of the same app is still compiling.

## Delivery credentials

While a build runs, the agent exposes a loopback endpoint for BuildKit to push
through. The endpoint holds the credentials for reaching the target device and
requires a password generated for that build alone. Other processes on the host
cannot use it to push their own image to the device.

The password is passed to BuildKit in a file with mode `0600`. It is not placed
on the command line, where other local users could read it through `/proc`.

## `status`

`status` reports what a build host can actually do, which is worth checking
before pointing a build at it:

```
Builder role: enabled
BuildKit:     available (v0.32.2)
Cache:        /data/buildkit/state (823.3 GiB free of 911.7 GiB)
Platform:     linux/arm64
Builds:       linux/arm64 natively
```

- **Builder role** — whether `enable` has been run.
- **BuildKit** — whether a buildkitd socket is present at
  `/run/buildkit/buildkitd.sock`, and the `buildctl` version found on `PATH`.
  BuildKit is installed out-of-band (distro package, release tarball, or not at
  all), so "which version is on that box" is otherwise invisible from the CLI.
- **Cache** — BuildKit's state directory and the free and total space on its
  filesystem. If it reports `inspection failed`, remote builds are refused
  until the cache location and available space can be determined.
- **Builds** — the platforms this host builds natively. Only its own platform is
  claimed. Emulated platforms are reported empty rather than guessed at, so a
  cross-architecture build is refused up front instead of silently running under
  QEMU and taking twenty minutes.

`status` needs neither the builder role nor BuildKit to answer — that is the
point of it. A device that is missing both still reports so.

## Cache space safety

Before starting a remote build, `wendy run --build-host` checks the free space
where BuildKit keeps its cache:

- Less than 8 GiB free: the build is refused.
- At least 8 GiB but less than 25 GiB free: the build proceeds with a warning.
- At least 25 GiB free: the build proceeds without a cache-space notice.

To move the cache, set buildkitd's `--root` option or put a top-level `root` in
the TOML file selected by its service, then restart buildkitd. For example:

```toml
root = "/data/buildkit/state"
```

The active file is not necessarily `/etc/buildkit/buildkitd.toml`: a service can
select another one with `buildkitd --config`. After restarting, run
`wendy device build-host status` and confirm that the `Cache` line names the
intended path and filesystem. If it reports `inspection failed`, inspect the
running buildkitd command and its active configuration rather than assuming the
default path.

## A Mac cannot be a build host

The Mac agent runs Linux containers through Apple Container, which has no
BuildKit underneath, so `status` on a Mac reports BuildKit unavailable and
`enable` cannot make it otherwise. A Mac remains a perfectly good build
*target*, and the intended machine to run `wendy run` from.

## Adopted Linux hosts

A non-WendyOS Linux machine that has been adopted into the mesh — a DGX Spark
running Ubuntu, say — is a legitimate build host, but Ubuntu ships no `buildkit`
package. Install the upstream release tarball and make sure `buildctl` is on
`PATH` by name (the agent runs it unqualified, and systemd units get a minimal
`PATH`, so a symlink into `/usr/bin` is the reliable placement). `status` then
reports the version it finds. If the host's root filesystem is small, configure
BuildKit's state directory on a larger filesystem before submitting builds.
