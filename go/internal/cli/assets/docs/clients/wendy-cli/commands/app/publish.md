# `wendy app publish`

Publishes an encrypted release of the app in `wendy.json` to the Wendy Cloud
registry. Every blob of the container image is encrypted on this machine;
Cloud stores ciphertext only.

```sh
wendy app publish --tag v1.4.0
wendy app publish --tag v1.4.0 --oci-layout ./image --platform linux/arm64
```

Without `--oci-layout` the project's container image is built first. The
release key is wrapped to this machine's registry key (see
[`wendy registry key`](../registry/key.md)) and to the Wendy custodian key.
The command prints the release id and manifest digest that
[`wendy app deploy`](deploy.md) takes.

| Flag | Description |
|------|-------------|
| `--tag` | Release tag, for example `v1.4.0` (required) |
| `--oci-layout` | Publish this OCI image layout directory instead of building |
| `--platform` | Image platform to build or pick from the layout (default `linux/arm64`) |
| `--dockerfile` | Dockerfile to build (default: detected) |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |

The registry is available where a custodian key is pinned (the development
cloud today); elsewhere the command refuses.
