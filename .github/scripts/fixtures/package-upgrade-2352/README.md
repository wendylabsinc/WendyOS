# Installed Linux package regression checks

`test-installed-linux-packages.py` consumes the actual CLI and Agent release
packages. It does not rebuild or modify the candidate artifacts. `prepare` makes
two test-only Agent packages, and `guest` runs actual package transactions and
Agent RPCs in a disposable, booted systemd-nspawn guest. Do not publish the
fixtures to a package repository.

The two shell files in this directory are byte-for-byte copies from commit
`68266f6b65251a7bab59ff66a4354f37636a6b7f`, the parent of the #2352 fix
`1f45f3390cffeb7cd8f8a40d0bcf8553921631d9`. Their hashes are checked before
fixture creation. The legacy package has version `0.0.0` and the current Agent
payload with those historical hooks. It tests the old upgrade ordering, not
compatibility with a historical Agent executable or its data format.

The DEB negative control changes only the new postinstall's Agent `restart` to
`try-restart`. The RPM negative control removes only the new posttrans recovery.
Each must complete its upgrade transaction and leave the enabled Agent stopped.
An installer failure, missing dependency, crashing Agent, or unbooted systemd
fails the check; none counts as detecting the negative control.

## Existing job integration

The shared scripts are the release recipe, also used by the PR gate:

```sh
bash .github/scripts/build-linux-release-cli.sh "$version" "$arch" "$cli_output"
bash .github/scripts/build-linux-packages.sh "$version" "$arch" \
  "$cli_binary" "$agent_binary" "$new_package_directory"
```

The CLI script preserves the pinned libusb archive, musl flags, version injection
and static-link check from `build-cli-linux`. The package script preserves the
package roots, metadata, dependencies, configuration declarations, scriptlets and
produced-package validation from `package-linux`. It creates all four packages in
a new output directory. It never installs them.

In `build.yml`, replace the libusb compile and CLI compile steps with the shared
CLI script; retain the native architecture selection, NOTICE copy, archive,
smoke checks and attestations. Replace package root construction, the two FPM
steps and metadata validation with the shared package script. Use a fresh child
directory for its output, then move those four packages to the existing upload
location. Keep attestation and upload steps after the guest check.

Use `ubuntu-24.04` for the existing amd64 package row and `ubuntu-24.04-arm` for
its existing arm64 row. The number of jobs does not change. Cross-packaging arm64
on x64 does not prove native execution and the guest runner rejects it.

The host needs Docker, systemd with PID 1, `systemd-container`, Python 3,
`ruby ruby-dev build-essential rpm cpio libarchive-tools`, and FPM. CLI builds
also need `musl-tools`, `pkg-config`, `curl` and `bzip2`. Existing release jobs
already supply most of these. Run the artifact and runner tests in the existing
Linux job:

```sh
python3 .github/scripts/installed_linux_packages_test.py
python3 .github/scripts/installed_linux_package_guests_test.py
```

The RPM/DEB roundtrips use real packaging tools and small nonexecuted ELF payloads.
They do not constitute installed Agent or repository acceptance evidence. On
hosts without RPM/FPM, that subset reports an explicit skip.

For actual release artifacts, after metadata validation and before attestation:

```sh
sudo env GITHUB_ACTIONS="$GITHUB_ACTIONS" RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \
  RUNNER_TEMP="$RUNNER_TEMP" \
  python3 .github/scripts/run-installed-linux-package-guests.py \
  --packages "$package_directory" --expected-version "$release_version" \
  --output "$RUNNER_TEMP/package-guests"
```

`release_version` is the version embedded in both Go binaries, not the RPM
version with punctuation changed. The package directory must contain exactly one
native package for each product/format. The runner prepares its own legacy/control
bundles. Fixtures stay outside the release artifact directory and are removed
when the run ends. No repository upload occurs here.

For required PR proof, add this command to the existing `linux-install-host`
job after its existing checks, using the same host dependencies:

```sh
bash .github/scripts/test-linux-packages-pr.sh "$RUNNER_TEMP/package-regressions"
```

It compiles one static release CLI and one CGO-disabled Agent from the current
checkout, using the same version, then calls the shared package recipe and guest
runner. The retained Make CLI used by the Stagefile gate has different dynamic
linkage and cannot substitute for the shipped static binary on Ubuntu 22.04.
Reuse the existing Go cache. These are required artifact builds, not another
runner or a dispatch of the release workflow. An amd64 PR run proves amd64 only;
arm64 remains subject to the native release package row. Budget 60 minutes for
the combined existing job; the guest runner has a 50-minute default maximum and
normally exits earlier. Hosted runtime must be measured before tightening that
budget.

## Guest provisioning and evidence

`run-installed-linux-package-guests.py` refuses execution outside a GitHub-hosted
Linux runner with root and systemd PID 1. It pulls immutable, native-architecture
Docker Library manifests for Ubuntu 22.04, Ubuntu 24.04 and Fedora 44. The index
and child hashes were verified against the registry on 2026-11-12. It only
creates/exports stopped Docker containers; it never starts a Docker container,
creates a builder or prunes shared images/cache.

For each distro, a non-booted nspawn root installs systemd/Python plus dependencies
read from the actual candidate packages. Apt resolves `Pre-Depends` and `Depends`;
DNF resolves the RPM requirements apart from RPM engine capabilities, which are
checked by the later real transaction. No Wendy binary is installed during this
network-connected phase. Exact installed dependency versions are retained.

The runner then makes a fresh root for each normal/negative-control case and
boots systemd with `--private-network`. There is no veth or host network access.
The verifier independently requires loopback to be the only network interface.
DNF local transactions use an empty repository directory. No dependencies are
silently ignored; a missing dependency fails the actual transaction.

Each nspawn process stays inside its unique transient systemd service through
`--keep-unit`, with delegated cgroups, `KillMode=control-group`, a runtime limit
and a bounded stop timeout. Cleanup verifies the full random ownership marker
before stopping the unit or removing its export container. It refuses to delete
roots with any remaining mounts. Host signals trigger bounded cleanup; the
service runtime limit also applies if the driver dies. Only owned roots and
containers are removed. Shared images/builders are retained.

The guest verifier requires its bundle nonce, native ELF architecture, unchanged
package hashes, no prior Wendy installation and a real systemd PID 1. It retains
the packaged Agent service and `Requires=containerd.service`. Do not mock
systemctl, replace that unit, suppress package scriptlets, use `--nodeps`, or
substitute another distro when provisioning fails.

The normal case checks the actual packaged CLI, the Agent executable hash,
local gRPC version/architecture, enabled service, changed service start identity,
stable health after upgrade, preserved local configuration and service shutdown
after removal. The negative case independently proves that the same installation
detects the old lifecycle failure. All six distro/case results are required.
A skipped guest, missing result, installer failure or cleanup failure is not a pass.

The output directory contains `summary.json`, candidate manifests and per-distro
image/provisioning logs, dependency inventories, boot logs, transaction logs,
Agent journals and `result.json`. Preserve the directory as a PR diagnostic
artifact even on failure. Release jobs should print the summary and bounded log
tails without adding diagnostics to the release provenance/upload sets. Do not
close #2352 until hosted normal and negative-control transactions have passed.

## Audit scope and remaining external evidence

#2504 changed the packaging runner to Ubuntu 24.04 to avoid RPMs rejected by
Artifact Registry. It was not a change to an Ubuntu glibc ABI. These native
installed-package checks add useful runtime compatibility evidence, but do not
prove Artifact Registry acceptance.

#2521 explicitly forced RPM format 4 and already added an assertion over the
first six bytes of every produced RPM. This fixture repeats that assertion for
its candidate and test packages and exercises the real local RPM reader. The
original audit's claim that no produced-artifact format assertion existed needs
correction, while the external acceptance gap remains open.

Keep Artifact Registry acceptance pending until an authorized release run shows
successful `gcloud artifacts apt upload` / `gcloud artifacts yum upload` for the
exact candidate hashes and actual apt/dnf consumption from the intended
repository. These tools create no repositories, upload no packages, and infer
no GCP acceptance from a successful local installation.
