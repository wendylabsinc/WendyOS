# 2245 non-UI simulator coverage

The original mixed PR contains separate corrections:

- `b3c4753939ee25e4171f86572b9fdbddf802e7e8`: reported degree limits must round-trip through pose acceptance, with radians clipped to the actual MuJoCo actuator range. Existing `test_reported_joint_limits_are_accepted` uses FastAPI TestClient and a substituted worker. New `test_real_http_joint_bounds_and_restart` uses actual loopback TCP/Uvicorn, production lifespan, real MuJoCo model/physics/render threads and exact JSON limits. It also proves invalid mixed commands are atomic, actual joint/frame advancement, pause, joined shutdown, failed HTTP while stopped, fresh same-endpoint restart and subsequent commands. `check_http_control.py` restores the exact old radians comparison and requires the named endpoint rejection, not an unrelated renderer/setup failure.
- `c2137da83eaafcecaea40fe6ea7d7cc1657ec08a`: the React component clears its own poll error after a successful request, while action errors remain separate. Backend HTTP recovery is not this UI state assertion. Keep the component poll-state acceptance pending; no new UI test is introduced here.
- `d5c064fe1` and `198926e36`: the actual viewer lock is packaged and native Linux CGO checks run. Existing Go bundle tests, shipped Docker asset hash tests and desktop packaging/type/service checks cover these layers. The native Linux SO101 workflow already enables CGO with its system dependencies and builds the shipped image. Actual packaged desktop execution remains distinct from source/cache tests.
- `c72c6452b`: vertical arrow keys on sliders are UI input behavior and remain outside this non-UI fixture.

Use the existing SO101 simulator job and its exact pinned shipped image. Its existing normal command mounts this tests directory and sets `SO101_RENDER_TEST=1`; the new test is automatically included in `pytest /tests`. Append the control to that same container invocation after the normal suite succeeds:

```sh
pytest -q -p no:cacheprovider /tests && python /tests/check_http_control.py
```

No new container image, runner, privilege, host device, browser, real arm, serial port or extra model download is needed beyond that job's existing build. The normal test takes two production startup/shutdown cycles and a short actual pose. Its two startup deadlines are 65 seconds each, individual HTTP requests 2 seconds, and joins 12 seconds. Both normal and old-bound control execute the journey in a dedicated Python process with a 180-second deadline; subprocess.run kills and reaps that exact child on timeout. The child launches only ASGI/MuJoCo threads. Socket setup failures close the listener, and failed startup checks both ASGI and simulator thread drain. Any uncertain thread cleanup fails and remains contained in that owned process. A failure cannot be accepted as an expected control unless the exact reported-bound assertion is present. Output renders stay in memory, never Git.

The existing `.github/workflows/so101-simulator.yml` already triggers on simulator, VM, MCP, desktop and Electron source changes and runs both native Linux rows and the existing viewer job. Its dependency filters also include the imported Go2 viewer source, shared app configuration, Go module pins, base TypeScript configuration, desktop test/scripts and package pins, and this registration document. The same existing workflow runs desktop and simulator checks on each matching change; no job is added.

Local syntax/source checks are not runtime acceptance. If the pinned SO101 model or native renderer is absent, keep real runtime and negative control pending until this existing image job executes them. A different cached MuJoCo version is not the shipped runtime proof. This fixture deliberately does not report React poll-alert recovery, keyboard behavior, packaged viewer loading or a physical SO-101 session as tested.
