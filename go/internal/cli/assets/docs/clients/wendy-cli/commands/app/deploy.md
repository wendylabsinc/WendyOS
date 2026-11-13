# `wendy app deploy`, `assign`, `unassign`, `rotate`

Deploys a release published with [`wendy app publish`](publish.md) to picked
devices through Wendy Cloud. Every request is signed with your operator key,
and each device gets its own signed instruction.

```sh
wendy app deploy --release <release-id> --name <name> --device <device-uuid> [--device ...]
wendy app assign --deployment <deployment-id> --device <device-uuid>
wendy app unassign --deployment <deployment-id> --device <device-uuid>
wendy app rotate --deployment <deployment-id> --device <device-uuid> [--device ...]
```

- **deploy** creates the deployment `--name` of the release at revision 1,
  running with the release's default configuration, makes its deployment key
  and sends each device its instruction. Each device's registry key is verified against
  your organization's certificate chain first; if any device has no verified
  key, nothing is created. `--digest sha256:<hex>` refuses a release whose
  manifest digest is not the one you expect.
- **assign** adds devices to an existing deployment, as one new revision.
- **unassign** removes devices, as one new revision; Cloud stops serving them
  the release.
- **rotate** replaces the deployment key and re-sends each `--device` its
  instruction at it, at the current revision. Name every device that should keep the deployment; devices not named
  stay on the old key. Only the deployment's creator can rotate.

Tag targets are not supported yet: a device cannot verify a tag that Cloud
resolved, so `--target-tag` is refused. Name each device with `--device`.

Assign and rotate need a deployment key wrapped to this machine: run them
from the machine that created the deployment.

| Flag | Description |
|------|-------------|
| `--release` | Release id (deploy, required) |
| `--name` | Deployment name, unique among the app's deployments (deploy, required) |
| `--deployment` | Deployment id (assign, unassign, rotate, required) |
| `--device` | Target device UUID (repeatable) |
| `--digest` | Expected manifest digest (deploy) |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |
