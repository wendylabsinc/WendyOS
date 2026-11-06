# WendyNotify

A Swift WendyOS example that serves a notification form at `http://<device>:8080/`.
The page has no login, so anyone who can reach port 8080 on that network can send notifications to the device's organization; a real app would put its own authentication in front.

It calls the local Agent over its private Unix socket using grpc-swift 2, without
WendyKit. Cloud accepts the notification for the Companion inbox and push delivery;
a successful creation does not confirm delivery to a phone.

From this directory, run:

```sh
wendy run --device <name>
```

Use a current WendyOS Agent with Cloud v2 notification forwarding. The included
`wendy.json` grants `notifications`, host networking, and HTTP port 8080. WendyOS
sets `WENDY_SYSTEM_SOCKET`; a missing socket is shown with its path and entitlement
requirement. No Cloud credentials belong in this app.

The page has user IDs (one per line), organization role checkboxes, title, body,
severity, device deep link, editable UUID v4, and optional JSON object metadata.
Selectors have union semantics. Cloud v2 refuses teams, so there is no team field.
The Agent validates the fields. Metadata JSON is parsed locally before sending.
Failures show the exact gRPC code and message or the specific local error. Entered
values stay in the form, including the ID, so errors can be corrected and retried.
After creation, use **Start a new notification** for a fresh UUID; reusing an ID
returns `ALREADY_EXISTS` rather than delivering again.

The last ten results are held in memory and disappear on restart. The rate panel
estimates the minute window from this app's accepted sends. It does not know about
previous runs or other instances. Cloud's rate-limit and quarantine responses carry
`retry after <ISO8601 timestamp>` in the error message; these timestamps are used
for the live countdown. The Agent's local burst refusal carries no retry time and
is never counted as a Cloud quarantine strike. Reload to update counts after a
countdown expires. No request is retried automatically.

Limits: 10 accepted notifications per minute per device app; three **Cloud
over-limit** refusals within ten minutes cause a 15-minute quarantine and a Wendy
Security notification to owners/admins. There are at most 100 selectors and 100
recipients, title is 1–120 printable UTF-8 bytes, body is 1–2000 printable UTF-8
bytes, and links must target the sending device, for example
`wendy://devices/current/live`. The Agent also enforces a short burst limit.

Deployment through Cloud works. The older CLI used for validation refuses port
8080 through Cloud v2, so page access with that CLI requires the device LAN.
Updated Cloud, CLI, and Agent versions support application ports as described in
the [Cloud tunnel guide](../../go/internal/cli/assets/docs/clients/wendy-cli/commands/cloud/tunnel.md#cloud-v2-ports).
Responses are not cached, cannot be framed, and send no referrer to external font
services.

## Development

```sh
swift build
swift test
```

To regenerate the checked-in client after changing the repository's
`Proto/wendy/system/v1/notifications.proto`, install `protoc`, then run this one
command from this directory:

```sh
./regenerate-grpc.sh
```

The script builds the SwiftProtobuf and grpc-swift-protobuf generators from the
resolved SwiftPM checkouts. Well-known types come from SwiftProtobuf. Normal
builds use the checked-in `.pb.swift` and `.grpc.swift` and need no `protoc`.
