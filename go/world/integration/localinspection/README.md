# Local inspection tools

Run from `go/world`. The builder uses Go from `PATH`, Wendy's parent module and
its toolchain requirement. It creates a fresh private output directory with the
local driver, mission gateway, commissioner and planar executor. It removes the
temporary driver package after building.

```sh
python3 -m integration.localinspection.build --output /absolute/private/tools
python3 -m integration.localinspection.run \
  --output /tmp/wendy-world-review-localinspection-demo \
  --driver /absolute/private/tools/localinspection-driver \
  --binaries /absolute/private/tools \
  --python /absolute/venv-with-mujoco-numpy-pillow/bin/python \
  --run-id local-demo
```

The output must be fresh and use the indicated `/tmp` prefix. The run is bounded
and cleans up only its owned processes. `--mode ready` with `--ui-command` retains
the separate native signing workflow; `--mode auto` cannot accept that argument.
This is a local simulation with generated fixture identities. It does not enroll
an agent or establish physical robot authority.
