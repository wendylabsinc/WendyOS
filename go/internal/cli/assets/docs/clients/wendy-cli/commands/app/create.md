# `wendy app create`

Creates an app in Wendy Cloud. It asks what the dashboard asks, in the same
order: what the app is called, what it does, and who has access to it. Then
it shows your answers; nothing is created until you confirm.

```sh
wendy app create
wendy app create --name "Line camera" --app sh.acme.line-camera --access ana@example.com=developer --yes
```

In a project folder, the name and app id start as the `appId` in
`wendy.json`. Each answer has a flag; with all of them nothing is asked.
Without a terminal, a missing `--name` or `--app` is an error, and `--yes`
is required.

| Flag | Description |
|------|-------------|
| `--name` | Name, shown in lists and on devices (required) |
| `--app` | App id: lowercase letters, digits, dots and hyphens. Use the same id in your project's `wendy.json` (required) |
| `--details` | What the app does |
| `--notifications` / `--no-notifications` | Whether the app may send notifications; people with access to it can receive them (default: none) |
| `--access <who>=<role>` | Give access on this app (repeatable). Who is a member's email, or a group as `<realm-issuer>#<group>`. Role is `owner`, `developer`, `operator` or `viewer` |
| `--yes` | Create without asking to confirm |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |

You are the app's owner. If your organization role doesn't allow
notifications, the app is created without them; ask an admin.

The app has no release yet. Publish its first one from the project folder:

```sh
wendy app publish --app <id> --tag 1.0.0
```
