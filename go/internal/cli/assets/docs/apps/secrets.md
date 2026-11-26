# App secrets in containers

Cloud deployment assignments deliver encrypted app secrets to the Wendy Agent.
The agent verifies each secret's app, name, version, key and device binding,
then exposes its decrypted bytes at `/run/secrets/<name>` before starting the
container. Read the file as bytes. Leading and trailing whitespace, LF and CRLF
are preserved, including multiline PEM values.

Each task gets its own private tmpfs. Files have mode `0400`, the directory has
mode `0500`, and both belong to the app's resolved runtime UID and GID. User
namespace mappings apply to that ownership. The mount is read-only with
`nodev,nosuid,noexec`. App mounts at or below `/run/secrets` conflict and are
refused. Limits are 64 secrets, 16 KiB per value and 64 KiB in total.

This implementation requires Linux tmpfs `noswap` support. [Linux 6.4 and later](https://www.kernel.org/doc/html/v6.4/filesystems/tmpfs.html)
provide it upstream; older vendor kernels need a backport. A mount or
decryption failure prevents startup. There is no disk-backed or swappable
fallback. Ordinary apps without secrets do not need this mount support.

Container metadata and deployment state retain ciphertext and device wraps
only. Secret plaintext is never added to the OCI environment. Apps can read
files directly, explicitly configure a supported `*_FILE` setting, or supply
their own entrypoint adapter for an application that requires environment
variables.

A changed secret set replaces the container and creates a fresh mount. An
equal-revision key rewrap updates the encrypted restart material without
restarting the running task. Stop, task exit, failed startup and removal discard
the mount. Agent startup removes orphan mounts and preserves mounts for live
tasks. Detach retains the running task and its encrypted restart material;
a later restart decrypts into a new mount.

The agent advertises `wendycloud.v2.DeviceAssignment.secrets` when its Linux
runtime has the secret opener wired. This consumes the contract from
[service-protos #215](https://github.com/wendylabsinc/service-protos/pull/215).
It does not add secret-management CLI commands.

## Runtime verification

The service tests decrypt the signed contract fixtures, including multiline
values, and reject malformed bindings. Linux mount tests check permissions,
read-only enforcement, separate mounts and cleanup:

```sh
WENDY_SECRETS_INTEGRATION=1 go test ./go/internal/agent/secretmount
```

The containerd lifecycle test uses a local busybox image and requires root,
`busybox-static`, a working containerd/runc and writable cgroups:

```sh
WENDY_CONTAINERD_INTEGRATION=/run/containerd/containerd.sock \
  go test ./go/internal/agent/containerd -run '^TestIntegrationAppSecretsLifecycle$'
```

Set `WENDY_SECRETS_SNAPSHOTTER=native` where nested overlay mounts are unavailable.
The test checks a non-root app's exact file reads and rejected writes, encrypted
metadata, restart, orphan recovery, failed decryption and task creation,
replacement with no secrets, and removal.
