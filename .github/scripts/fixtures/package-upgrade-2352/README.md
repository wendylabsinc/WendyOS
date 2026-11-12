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

Use the existing `package-linux` job after package metadata validation and before
attestation/upload. FPM, RPM and dpkg are already present there. Add Python 3 and
`libarchive-tools` for `bsdtar`, plus `cpio` for FPM's RPM input. Reuse its exact
produced packages and version:

```sh
python3 .github/scripts/test-installed-linux-packages.py prepare \
  --format deb --agent "$agent_deb" --cli "$cli_deb" \
  --expected-version "$release_version" --output "$RUNNER_TEMP/package-deb"
python3 .github/scripts/test-installed-linux-packages.py prepare \
  --format rpm --agent "$agent_rpm" --cli "$cli_rpm" \
  --expected-version "$release_version" --output "$RUNNER_TEMP/package-rpm"
```

`release_version` is the unmodified version embedded in both Go binaries, not the
RPM version with punctuation changed. Outputs must not already exist. Store the
fixture bundles as temporary job files, separate from release artifacts.

Run `python3 .github/scripts/installed_linux_packages_test.py` in the existing
Linux package test step. Its DEB/RPM roundtrips exercise the actual packaging
tools. Its small ELF payloads are never executed and do not constitute installed
Agent, ABI, systemd, or repository acceptance evidence. On hosts without RPM/FPM,
the RPM roundtrip reports an explicit skip.

For transaction proof, prepare a fresh native-architecture distro root for
**each** format/case combination. Reuse the existing Linux install/package
runner; do not add a matrix job or a new runner just for these checks. Ubuntu
22.04 and 24.04 cover the documented oldest recommended Ubuntu and the packaging
runner distribution. A supported Fedora root covers the RPM consumer. Record
its exact release and immutable source image digest in job evidence; do not
silently substitute another release if provisioning fails. Both amd64 and arm64
require native execution. An x64 runner packaging arm64 does not establish
arm64 runtime compatibility.

The guest needs Python 3, a bootable systemd installation, D-Bus, containerd,
xdg-dbus-proxy, CA certificates, USB utilities and Avahi. Use `avahi-daemon` on
Ubuntu and `avahi` on Fedora. Preinstall these dependencies before disconnecting
its network. Containerd must be able to start in the guest's delegated cgroup
and mount namespaces. Retain the actual packaged Agent service and its
`Requires=containerd.service`; do not replace it, mock systemctl, suppress
scriptlets, or use `--nodeps` to get a passing result.

Boot each unique, task-owned root with systemd-nspawn. Bind its bundle read-only
at `/fixture`, and a new host evidence directory at `/evidence`. Use
`--private-network` so the actual Agent cannot enroll or discover outside peers.
Once that machine is running, the exact guest invocation is:

```sh
nonce=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["nonce"])' "$bundle/manifest.json")
sudo systemd-run --machine="$machine" --wait --pipe --collect \
  --service-type=exec --setenv="WENDY_PACKAGE_GUEST_NONCE=$nonce" \
  /usr/bin/python3 /fixture/test-installed-linux-packages.py guest \
  --bundle /fixture --nonce "$nonce" --case "$case" \
  --evidence "/evidence/$case"
```

`case` is `normal` or `negative-control`. The evidence directory for that case
must not exist. The guest refuses non-Linux/non-root execution, a PID 1 other
than systemd, a non-nspawn container, a wrong nonce, a non-native architecture,
modified package bytes, or pre-existing Wendy installations. These checks are
accident-prevention guards, not a sandbox for untrusted artifacts.

The host must apply a bounded overall deadline and a trap that terminates only
that unique machine, waits for it to exit, and removes only its temporary root.
Guest subprocess timeouts do not guarantee that apt/dnf descendants have exited;
terminating the guest is the final cleanup boundary. Always retain or print
`result.json`, transaction logs and `agent-journal.log`, including on failures.
A missing result or a skipped guest is not a pass.

The normal case checks the actual packaged CLI, the Agent's executable hash,
local gRPC version/architecture, enabled service, a changed service start
identity, stable health after upgrade, preserved local configuration and
service shutdown after package removal. The negative case independently proves
that the same installation detects the old lifecycle failure. Both cases must
pass before #2352's transaction coverage is considered verified.

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
