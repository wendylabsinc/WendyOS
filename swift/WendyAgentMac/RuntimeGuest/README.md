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

## Clean bootstrap regression

From the repository root, run:

```sh
python3 .github/scripts/test-runtime-guest-bootstrap.py --output /tmp/new-runtime-evidence
```

This requires Go selected by the root `go.mod`, Python with PyYAML, `cpio`,
`bsdtar`, and a reachable ordinary Docker-driver builder for the current named
context. It refuses an emulated architecture or a docker-container builder.
It creates no builders and changes no Docker defaults. On Linux, the additional
packages are `cpio libarchive-tools python3-yaml`; Docker and Go are already
provided by the existing Linux install CI job.

The fixture reads the runtime release job's actual setup-go file references,
copies only the tracked build/proxy sources and root module files to an isolated
workspace, and runs the unchanged `build.sh` with empty private Go caches and
BuildKit's no-cache option. It checks the kernel header and matching module tree,
newc initramfs, packaged configuration and init script, APK inventory, and the
proxy's actual Go build information. It executes BuildKit/containerd/nerdctl/runc
version commands and the proxy's invalid-forward path from the produced rootfs
inside ordinary read-only, network-disabled containers. `/init` is not run.

The existing Linux install job can run this for native amd64; the current
runtime release job is native arm64. The artifact checks do not prove VM boot,
Virtualization.framework integration, vsock transport, or hardware behavior.
The separate Fumadocs job retains `npm ci` and `npm audit --audit-level=high` for
the dependency portion of #2127.

The release job can add `--retain-artifacts "$RUNNER_TEMP/runtime-artifacts"`
and archive the checked files from that new directory after the command succeeds.
This reuses the exact built bytes instead of compiling another guest. A failed
command must never publish retained files.

Output contains build/tool logs, the exact Docker build arguments and
`evidence.json` with artifact hashes, kernel/module version, proxy build details,
and installed package inventory. Temporary source/module caches and owned
containers are cleaned up. Docker's shared content cache and images remain.
The portable `runtime_guest_bootstrap_test.py` suite rejects the original nested
Go paths, wrong kernel/module pairs, malformed archives, and builder mutations.
No new CI job is needed.
