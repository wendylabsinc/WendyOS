2049 actual kernel driver lifecycle

Run from the WendyOS checkout in one existing GitHub-hosted Linux x86_64 job:

  python3 .github/scripts/test-driver-lifecycle-vm.py \
    --work-parent "$RUNNER_TEMP" --output "$RUNNER_TEMP/driver-lifecycle-vm"

Use the containerd 1.7 matrix row only, or another single existing Linux row.
This test does not depend on the host containerd version. Do not duplicate it
across both containerd rows or add a runner. Required host tools are ordinary
Docker, Go from the repository, Python3, curl, qemu-system-x86_64, qemu-img,
e2fsprogs, tar, findmnt and noninteractive sudo for validated private rootfs
extraction/mkfs/removal. The host runs the coordinator as its ordinary user.
No host module load/unload, sysext merge, mount, service change, device access,
KVM, tap, bridge or guest networking is performed. Kernel mutations happen
only inside the disposable QEMU kernel. Do not bypass the hosted admission.

The guest root is the same native Fedora 44 digest pinned by the existing package
checks. A matching kernel-core/kernel-devel pair and build dependencies are
resolved in its ordinary Docker build. Exact RPM versions, kernel/initrd hashes
and every packed .raw hash are retained. Mismatched versions or missing actual
systemd-sysext mutable/always-refresh support fail preparation. This is a generic
kernel integration fixture, not an assertion about a vendor kernel's ABI.

Both pack-sysext.sh and wendyos-sysext-apply.sh come byte-for-byte from the pinned
published WendyOS-Builder commit f72a1b354c80523dc2b092e52e38e44fe56c8d5a.
The driver checks their SHA256 before execution. That apply script is unchanged
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
each boot. These are failure bounds, not measured runtime. First hosted execution
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
