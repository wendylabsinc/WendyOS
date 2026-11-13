# Disposable review apps

These fixtures provide synthetic logs and app states for the Wendy ChatGPT
review walkthrough. They were generated with `wendy init` and use only Python's
standard library. They have no hardware access, network access, listeners,
persistent volumes, or file writes. Each container runs as UID 65534.

- `review-logger` emits JSON INFO heartbeats every five seconds and a WARN at
  startup and every fifteen seconds. Every record has `synthetic: true`.
- `review-helper` emits only INFO heartbeats. Leave it running while stopping
  `review-logger` in the app-control review case.
- `review-stopped` is an optional second heartbeat fixture to stop before
  recording, so app inventory includes a stopped app.

The warnings are test data and do not describe a device fault. A startup
`Synthetic review fixture ready; stdout only` record and later heartbeat
establish that the fixture is producing output. These apps have no HTTP health
endpoint. SIGTERM and SIGINT trigger a final clean-stop record and exit code 0.

## Deploy to the selected review Orin

Run from this plugin directory. These commands target `joannis-agx-orin` at
`192.168.100.126:50052`. Reconfirm that address before reusing the fixtures on
another day. The CLI below is a local build from the current checkout. Earlier
preparation used `ci-pi5`; the current walkthrough uses the Orin.

```sh
/private/tmp/wendy-chatgpt-demo --json run --yes --detach --no-restart --skip-cloud-registration --build-type docker --device 192.168.100.126:50052 --prefix review-fixtures/review-logger
/private/tmp/wendy-chatgpt-demo --json run --yes --detach --no-restart --skip-cloud-registration --build-type docker --device 192.168.100.126:50052 --prefix review-fixtures/review-helper
/private/tmp/wendy-chatgpt-demo --json run --yes --detach --no-restart --skip-cloud-registration --build-type docker --device 192.168.100.126:50052 --prefix review-fixtures/review-stopped
wendy device apps stop review-stopped --device 192.168.100.126:50052
```

The last two commands start the optional fixture and then stop it. Docker may
download the base image during the build. The apps themselves need no network
access. During earlier Pi preparation, `--deploy` failed in the registry push
before creating the container; the commands above use the verified deployment
path.

## Check the demo and reset it

```sh
/private/tmp/wendy-chatgpt-demo --json device apps list --device 192.168.100.126:50052
/private/tmp/wendy-chatgpt-demo --json device logs --app review-logger --tail 20 --no-follow --device 192.168.100.126:50052
/private/tmp/wendy-chatgpt-demo --json device logs --app review-helper --tail 10 --no-follow --device 192.168.100.126:50052
```

During the review, stop only `review-logger`, then read back app states and
confirm that `review-helper` is still running. To rehearse this outside ChatGPT:

```sh
/private/tmp/wendy-chatgpt-demo device apps stop review-logger --device 192.168.100.126:50052
/private/tmp/wendy-chatgpt-demo --json device apps list --device 192.168.100.126:50052
```

Reset the logger for another recording by repeating its deploy command above.
That restores the no-restart policy and resets its in-memory sequence counter.
The Agent may retain previous logs, so use timestamps to identify the new run.

The public plugin packager excludes this entire `review-fixtures` directory.
