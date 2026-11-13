# First Stagefile app

A Flask teaching server with `GET /` returning `{"message":"Hello from Stagefile"}`
and `GET /health` returning `{"status":"ok"}`. The Python entrypoint starts the
server on port 8000; the image runs as non-root UID 65532.

Build locally without deploying:

```sh
wendy project validate .
wendy build --device docker
```

Use `wendy discover` to find a Linux Wendy target. Replace `my-device.local`
with its hostname, address, or `vm:name` selector:

```sh
WENDY_TARGET=my-device.local
wendy run --device "$WENDY_TARGET"
```

Open the printed URL and its `/health` path. TCP readiness proves a listening
server; inspect the HTTP responses to verify this app. Press Ctrl-C to leave the
attached run, then stop the app explicitly:

```sh
wendy device apps stop sh.wendy.examples.stagefile-getting-started --device "$WENDY_TARGET"
```

Ctrl-C can leave an app running when the command only followed existing logs.
Repeat the explicit stop after each experiment.

Change `MESSAGE` in `app.py`, repeat `wendy run --device "$WENDY_TARGET"`, and
verify the changed response. With a
warm, eligible Wendy device deployment cache, the CLI can report
`App layer(s) rebuilt natively (deps unchanged; container builder skipped)`.
A local `wendy build --device docker` uses the image builder and does not exercise
that deployment shortcut. Normal cached builds are also valid.

Keep the generated Stagefile lockfile when pinning the example for your project.
Do not edit or commit `Dockerfile.generated*`. Flask's development server is used
for this teaching exercise, not as a production serving recommendation.
