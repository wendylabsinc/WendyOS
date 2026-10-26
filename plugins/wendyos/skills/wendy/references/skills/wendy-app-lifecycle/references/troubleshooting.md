# App troubleshooting

## Port conflicts

With host networking, apps on a device share listening ports. A simulator's
forwarding error can instead concern a port on the developer's host. Find the
owner in startup logs and the app list, and leave unrelated listeners running.

To move an app's listener, change the server's port, its `http` entitlement,
explicit port mappings, `hooks.postStart.openURL`, and a
`readiness.tcpSocket.port` that probes it; keep a probe for a separate health
port unchanged. Then run `wendy json validate`, redeploy to the same device, and
check the response on the new port. A passing TCP probe alone can come from the
old listener.

## Debugger

`wendy run --debug` starts the app under a debugger. A Python image must already
bundle `debugpy`; the CLI does not inject it. Swift apps attach through the
WendyOS VS Code extension (`wendy docs remote-debugging/vscode-extension`).

## Telemetry

Apps send OpenTelemetry over OTLP/HTTP, not gRPC, to the device's collector:

```bash
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
```

`wendy device telemetry-stream --logs --app <app-id> --device <selector>` streams
JSONL logs; `--metrics` and `--traces` add those streams, and no selector
enables all three.
