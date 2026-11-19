# `wendy app promote`

Turns an app you ran directly with [`wendy run`](../run.md) into a deployed
release. Promote publishes the image the last `wendy run` of this project
built, as [`wendy app publish`](publish.md) would, and then asks the
[`wendy app deploy`](deploy.md) questions with the app, the release and the
device already answered.

```sh
wendy app promote --device packer-01 --tag 1.0.0
wendy app promote --device packer-01 --untagged --name bench --run --yes
```

Promote publishes the image `wendy run` keeps in its build cache for this
app and platform, so it does not build again. If there is none (no
`wendy run` yet, or a builder that keeps no cache, such as Apple
Container), promote stops: run the app with `wendy run` first, or publish it
with `wendy app publish`.

The device replaces the running app with the published release. It does not
keep the running container yet.

| Flag | Description |
|------|-------------|
| `--device` | The device you ran the app on: its name or id. Asked when missing in a terminal |
| `--tag` | Release tag: the version you give this release, for example `1.0.0`. Asked when missing in a terminal |
| `--untagged` | Name the release `<git branch>@<manifest digest>` instead of a tag |
| `--platform` | Platform of the run's image (default `linux/arm64`) |
| `--name`, `--move`, `--config`, `--config-file`, `--run`, `--install-only`, `--keep-trying`, `--note`, `--yes`, `--wait` | The remaining deploy questions, as in [`wendy app deploy`](deploy.md) |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |
