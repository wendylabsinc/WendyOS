# `wendy app publish`

Publishes an encrypted release of the app in `wendy.json` to the Wendy Cloud
registry. Every blob of the container image is encrypted on this machine;
Cloud stores ciphertext only.

```sh
wendy app publish --tag 1.0.0
wendy app publish --untagged
wendy app publish --tag 1.0.0 --oci-layout ./image --platform linux/arm64
```

If the app does not exist in Cloud yet, publish first asks the
[`wendy app create`](create.md) questions (in a terminal) and creates it.

The image carries the project's `wendy.json` as its app definition, and
the release's default configuration is the `env` object of `wendy.json`.
Publish refuses, before uploading anything, what a device would refuse: an
app with several services, a `wendy.json` with any warning, or an app
definition over 64 KiB.

Without `--oci-layout` the project's container image is built first. The
release key is wrapped to this machine's registry key (see
[`wendy registry key`](../registry/key.md)) and to the Wendy custodian key.
The command prints the release id and manifest digest that
[`wendy app deploy`](deploy.md) takes.

| Flag | Description |
|------|-------------|
| `--tag` | Release tag: the version you give this release, for example `1.0.0`. Asked when missing in a terminal |
| `--untagged` | Name the release `<git branch>@<manifest digest>` instead of a tag |
| `--app` | App id; must match `appId` in `wendy.json` (default) |
| `--oci-layout` | Publish this OCI image layout directory instead of building |
| `--platform` | Image platform to build or pick from the layout (default `linux/arm64`) |
| `--dockerfile` | Dockerfile to build (default: detected) |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |

The registry is available where a custodian key is pinned (the development
cloud today); elsewhere the command refuses.
