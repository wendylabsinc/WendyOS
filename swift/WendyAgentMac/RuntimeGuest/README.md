# WendyAgentMac local BuildKit guest

This immutable Linux initramfs backs the optional macOS build service. It runs
three long-lived services:

- containerd, with its content and snapshot roots on the persistent data disk;
- BuildKit, using the containerd worker and the `wendy` namespace;
- a small vsock proxy exposing only BuildKit on port 6237.

The host maps BuildKit to a user-owned Unix socket. Build contexts and OCI
outputs flow through BuildKit's session protocol. The service does not run
Wendy applications; Apple `container` remains the macOS application runtime.

`build.sh` builds the architecture-specific initramfs into `../Resources/runtime`
using a reachable BuildKit daemon. The kernel and its modules come from the
same Alpine `linux-virt` package transaction: the modules remain in the
initramfs and the kernel is copied beside it for Virtualization.framework to
boot directly. APK repository signatures cover that transaction, and release
packaging can additionally pin the resulting artifact digests.
