`wendy project entitlements add [type]` adds a capability to `wendy.json`.
The shorter `wendy project add [type]` also accepts integrations such as `ros2`.

Omit the type to open a searchable picker. Interactive forms collect missing
values and show a change preview before saving. For scripts, supply required
values explicitly:

```sh
wendy project add http --port 8080
wendy project add persist --name recordings --path /data
wendy project add i2c --bus i2c-1
```

Use `--dry-run` to preview without saving, or `--service <name>` for a service's
settings. See the [project editor](../index.md) and
[entitlement reference](../../../../../apps/wendy.json.md#entitlements-1).
