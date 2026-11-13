# `wendy app deploy`, `deployment`, `assign`, `unassign`, `rotate`

Deploys a release published with [`wendy app publish`](publish.md) to devices
through Wendy Cloud. It asks the same questions, in the same order, as the
dashboard's "Deploy an app": which app, which release, which devices (and the
deployment's name), what configuration, and whether it should run now. Then
it shows a review and, once you confirm, sends each device its own instruction
signed with your operator key. Every question has a flag; with all of them
given nothing is asked. Without a terminal, a missing answer is an error that
names its flag.

```sh
wendy app deploy                     # asks each question
wendy app deploy --app <id> --release <version> --tag <tag> --device <name> \
  --name <deployment> --config NAME=VALUE --run --yes
wendy app deployment show <app>/<deployment>
wendy app assign --deployment <deployment-id> --device <device-uuid>
wendy app unassign --deployment <deployment-id> --device <device-uuid>
wendy app rotate --deployment <deployment-id> --device <device-uuid> [--device ...]
```

- **deploy** verifies each chosen device's registry key against your
  organization's certificate chain first; if any device has no verified key,
  nothing is created. A device that already runs the app in another
  deployment moves only with `--move` (or your confirmation). Progress shows
  each device as running, installed, downloading, waiting or failed, with the
  reason; `--wait` stays until every device is done.
- **deployment show** shows a deployment and each device's state.
- **assign** adds devices to an existing deployment, as one new revision.
- **unassign** removes devices, as one new revision; Cloud stops serving them
  the release.
- **rotate** replaces the deployment key and re-sends each `--device` its
  instruction at it, at the current revision. Name every device that should keep the deployment; devices not named
  stay on the old key. Only the deployment's creator can rotate.

Assign and rotate need a deployment key wrapped to this machine: run them
from the machine that created the deployment.

| Flag | Question |
|------|----------|
| `--app` | 1 Which app? |
| `--release` | 2 Which release? (its version or id) |
| `--device`, `--tag` | 3 Which devices? (repeatable) |
| `--name` | 3 The deployment's name |
| `--move` | 3 Move devices from another deployment of the app |
| `--config NAME=VALUE`, `--config-file` | 4 What configuration? The release's defaults otherwise |
| `--run`, `--install-only` | 5 Should it run now? |
| `--keep-trying` | 5 How long offline devices keep trying: 7d, 30d, 60d (default) or 1y |
| `--note` | 6 Note for the history |
| `--yes` | 6 Deploy without asking for confirmation |
| `--wait` | 7 Stay until every device is done |
| `--digest` | Refuse a release whose manifest digest is not this one |
| `--deployment` | Deployment id (assign, unassign, rotate) |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |
