2049 actual kernel driver lifecycle

Run from the WendyOS checkout in one existing GitHub-hosted Linux x86_64 job:

  python3 .github/scripts/test-driver-lifecycle-vm.py \
    --work-parent "$RUNNER_TEMP" --output "$RUNNER_TEMP/driver-lifecycle-vm"

Run once in the existing containerd-regressions job, after the exported ROS
image/control cleanup and before ESP-IDF pulls its large toolchain image. That job loops over 1.7/current internally; this VM
must stay outside that loop because it does not use host containerd. Do not
add a matrix or another runner. Set this step timeout to 30 minutes, keeping
the existing job timeout of 50 minutes. Add qemu-system-x86, qemu-utils and
e2fsprogs to the existing apt dependency installation. Required host tools are ordinary
Docker, Go from the repository, Python3, curl, qemu-system-x86_64, qemu-img,
e2fsprogs, tar, findmnt and noninteractive sudo for validated private rootfs
extraction/mkfs/removal. The host runs the coordinator as its ordinary user.
No host module load/unload, sysext merge, mount, service change, device access,
KVM, tap, bridge or guest networking is performed. Kernel mutations happen
only inside the disposable QEMU kernel. Do not bypass the hosted admission.

The guest root is the official Fedora 45 amd64 manifest
sha256:53d138f25f6738257c78553d51369b662efc0e0eb1cd1556a2fe0ad5a115be88,
published 2026-11-02 from fedora-cloud/docker-brew-fedora source
a980be0a4c57354f94e13f0e70dd859ec3144ac8. The registry manifest bytes and config
digest were verified; its config declares linux/amd64 and Fedora 45. This guest
requires systemd 260 or later because the UNCHANGED production Builder script
uses --always-refresh, introduced in 260. Fedora 44/systemd 259.10 failed this
prerequisite before any kernel test. Fedora's official Fedora 45 package data publishes
systemd 262.1-1.fc45 (2026-10-17):
  https://packages.fedoraproject.org/pkgs/systemd/systemd/fedora-45-updates.html
  https://www.freedesktop.org/software/systemd/man/262/systemd-sysext.html
A matching kernel-core/kernel-devel pair and build dependencies are
resolved in its ordinary Docker build. Exact RPM versions, kernel/initrd hashes
and every packed .raw hash are retained. Mismatched versions or missing actual
systemd-sysext mutable/always-refresh support fail preparation with the actual
RPM versions, binary version and missing prerequisite printed in the build log.
The feature check reads actual installed binary help after package installation;
a version number alone cannot substitute for the required options. Packages are
resolved at build time and their exact versions retained; the image digest does
not freeze the repository's RPM updates. The Fedora 45 full image build and VM
lifecycle still require hosted execution; registry/package verification is not
kernel proof. This is a generic kernel integration fixture, not an assertion about a vendor kernel's ABI.

Both pack-sysext.sh and wendyos-sysext-apply.sh come byte-for-byte from the pinned
published WendyOS-Builder commit f72a1b354c80523dc2b092e52e38e44fe56c8d5a.
Both exact URLs were fetched without credentials or curl configuration and
returned HTTP 200 with matching hashes during preparation. No cross-repository
GitHub token is required. The driver checks their SHA256 before execution. That apply script is unchanged
from production source commit 1d2c171e054a4a52bc34954c2f112c2c86ef0c89; the
package recipe is unchanged from 9c8a3f672fce0f94ce8d7824a8e3956644a6b5df.
No mock modprobe, fake sysfs, manufactured receipt or substituted merger is used.
The native module contains only a read-only version parameter and optional
failing init. It binds no physical device and performs no I/O or background work.

The actual DriverService defaults run behind a local Unix gRPC server. The test
streams genuine squashfs images and checks:

- installation activates real module version 1 and truthful loaded status;
- replacement unloads/reloads version 2, changes the runtime receipt and reports
  no reboot required, with the healthy neighbour still operational;
- a version 3 module whose real init returns EIO fails installation, restores the
  version 2 image bytes and actually runs version 2 again;
- removal stays committed despite another staged add-on failing real module init;
  the removed image/exposure disappears while its still-resident code is honestly
  reported as requiring reboot;
- a second QEMU invocation boots the same disk with a new kernel boot ID. The
  removed module stays absent while the remaining healthy add-on loads through
  the real boot apply script.

Fresh QCOW overlays then run two controls independently. Omitting only the
runtime's unload call must fail the live version 2 assertion. Restoring the old
Go unconditional-removal rollback policy must fail removal beside the broken
neighbour. A compile error, prerequisite skip, kernel panic, timeout or unrelated
failure is never accepted as a rejected regression.

Preparation requires 16 GiB free in addition to the caller's existing work. The
root archive is capped at 3 GiB expanded and 400,000 entries, the base disk at 4 GiB,
and each of the three overlays is created and removed sequentially. QEMU uses
2 GiB RAM and two TCG vCPUs. Per-command ceilings are 10 minutes for each Go binary,
20 minutes for the ordinary image build, 3 minutes for export/extraction/mkfs and 10 minutes for
each boot. All preparation and four boots also share one 25-minute work deadline;
each command receives the smaller of its own limit and the remaining budget.
Cleanup has a separate three-minute reserve. The 30-minute workflow step
bound allows command-group draining and evidence upload. The existing job
completed in 8 minutes 32 seconds at head 4f2f0d9aa (job 131626456277), including
both containerd versions, ROS scope controls and ESP-IDF. This leaves room for
the single driver gate without increasing the existing 50-minute job budget.
The first driver run has not been measured, so 25 minutes is an execution
budget rather than a runtime claim. First hosted execution
must establish actual duration/capacity before changing the audit to verified.
The workflow's existing job timeout remains the outer bound. A smaller runner
that cannot meet the preflight remains a failed prerequisite, not a skipped pass.

The shared pinned-process guardian owns command groups and waits for them on
interrupt/timeout. The VM exposes no host directory. Archive validation rejects
traversal, devices, duplicate paths, outside hardlinks and any descendant of a
symlink before the only privileged extraction. No host mount is created, and
cleanup independently rejects any mount below the owned rootfs. Docker cleanup
checks exact nonce labels and removes only its stopped container and tagged
image. Shared Docker/build caches are never pruned. Uncertain cleanup retains
owned work and failed evidence. Preserve the small output directory with an
always-run artifact step in the existing job, including failures. Do not upload
or commit VM disks, module binaries or .raw images.

Local validation currently consists of native-source review, Linux test-binary
compilation and portable path/evidence/cleanup admission tests. No VM, real kernel
module, sysext transition or physical device execution is claimed by those checks.
Physical PCI/USB binding, radio/firmware state and vendor-specific module stacks
remain covered only by their separate target procedures.

Fedora system-bus fixture prerequisite

Hosted run43360636095/job131673068153 booted Fedora45 but dbus.socket
repeatedly failed before InstallDriver. The image installed dbus-daemon with
weak dependencies disabled and did not enable a system-bus provider.
The exact Fedora1.16.2-6.fc45 packages were downloaded from:
https://dl.fedoraproject.org/pub/fedora/linux/development/45/Everything/x86_64/os/Packages/d/dbus-daemon-1.16.2-6.fc45.x86_64.rpm
https://dl.fedoraproject.org/pub/fedora/linux/development/45/Everything/x86_64/os/Packages/d/dbus-common-1.16.2-6.fc45.noarch.rpm
Their SHA256 values were respectively:
519e018add2f8352f126d718a1e48561b0513b480956ee29d1101485b76d5045
7bf6865f6d9f270c14143b3e6cb78ec9bb7242014fe0c2c3cee7656716d98e97
Extracted dbus.socket uses its default dbus.service name. The installed
dbus-daemon.service declares Alias=dbus.service messagebus.service under
[Install]. Explicitly enabling that installed unit creates the required alias;
the image build verifies it resolves to the exact daemon unit. No host service
is started during the ordinary image build.

Before any driver mutation the guest now requires successful system-bus GetId
and systemd Peer.Ping calls with finite command deadlines. Missing or unhealthy
services produce bounded unit status/journal/config diagnostics and a failed
nonce-bound guest exit. The original activation, module-version, rollback and
reboot assertions remain unchanged. Portable tests execute inert command
children, including a real stalled child; only hosted QEMU can establish the
actual bus and driver lifecycle pass.
