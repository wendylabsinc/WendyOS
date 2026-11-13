# `wendy app release`

Records a release of the app in `wendy.json` in Wendy Cloud: a tag naming one
image manifest digest.

```sh
wendy app release --tag v1.4.0 --digest sha256:<64 hex>
```

Both flags are required. A release is never implicitly `latest`, and
deployments pin the digest, not the tag. Each release carries one tag.

| Flag | Description |
|------|-------------|
| `--tag` | Release tag, for example `v1.4.0` (required) |
| `--digest` | Image manifest digest, `sha256:<64 lowercase hex>` (required) |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |

The release is signed with your operator key and created in your session's
organization.

To record a deploy as it happens, use `wendy run --record-release <tag>`; see
[`wendy run`](../run.md#recording-a-release).
