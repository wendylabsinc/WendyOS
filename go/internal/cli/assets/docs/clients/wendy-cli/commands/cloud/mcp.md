# `wendy cloud mcp`

Lets a web chat (ChatGPT, Claude) use Wendy through your own CLI session.

## Connect a web chat

1. Sign in and start a Wendy session in your terminal:

   ```sh
   wendy cloud login
   wendy cloud mcp
   ```

2. Add the Wendy connector in your web chat and sign in to the same organization.
3. Ask the chat to check Wendy's status. Compare the pairing code in the chat
   with the code in your terminal, then accept only if they match.
4. Keep the terminal open. Press Ctrl+C when you want the chat's access to end.

Your organization must allow MCP and Hosted MCP. One client connection can pair
with each running CLI session; another client needs a separate session.

## Choose what the chat can do

By default, built-in device and Cloud read operations are available and ask for
terminal confirmation. To approve these reads automatically:

```sh
wendy cloud mcp --allow-read
```

To also permit deployment, while still confirming each deployment:

```sh
wendy cloud mcp --allow-read --allow-tool run
```

Host access, local discovery, and app-provided tools need explicit permission;
`--allow-read` does not approve them. The CLI reports unknown tool names and lists
the available choices. A call cannot switch to another Cloud organization or
endpoint behind your back.

Use `--allow-all` only when you want all supported operations, including host
access and device changes, to run without per-call confirmation. Pairing remains
a separate step. `--auto-accept` skips matching-code approval for ChatGPT's known
client only; it does not change tool permissions.

If pairing reports an incompatible tool catalog, update the Wendy CLI and restart
the session. The chat receives the tools supported by the hosted service; it may
not expose every tool in a newer CLI yet.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--cloud-grpc` | `""` | Cloud gRPC endpoint of the session to use. |
| `--allow-tool` | none | Also allow these tools by name, comma-separated. Each call requires confirmation unless it qualifies for `--allow-read`. |
| `--allow-read` | `false` | Skip confirmation for built-in read-only device and Cloud tools that do not access the host. |
| `--allow-all` | `false` | Allow every supported relay tool without per-call confirmation. |
| `--auto-accept` | `false` | Automatically accept pairing from ChatGPT's known OAuth client. Tool permissions and confirmations are separate. |

## Examples

Pair a web chat with read-only access, confirming each call:

```sh
wendy cloud mcp
```

Allow built-in device and Cloud reads without confirmation:

```sh
wendy cloud mcp --allow-read
```

Also let it deploy, confirming each deploy:

```sh
wendy cloud mcp --allow-read --allow-tool run
```

Allow every supported tool without further tool confirmations after pairing:

```sh
wendy cloud mcp --allow-all
```

Automatically pair ChatGPT and allow built-in device and Cloud reads without confirmation:

```sh
wendy cloud mcp --auto-accept --allow-read
```
