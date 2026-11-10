Renders a TUI dashboard for your device, showing OTel metrics and logs in real time.

## Flags

| Flag | Description |
|------|-------------|
| `--app <name>` | Filter the dashboard to a single application. |

## Key bindings

The footer lists the available keys:

```
q/Ctrl+C exit | ↑/↓ scroll | ←/→ pan logs | G/g end/start
```

> Looking for the app-count status line (`3 apps  ● 2 running  ○ 1 stopped`)?
> That belongs to the interactive app list, [`wendy device apps list`](./apps/list.md),
> not to this dashboard.
