# Wendy for ChatGPT

## Hosted plugin

The public package connects to the Wendy CLI relay at `https://mcp.wendy.dev`
using Streamable HTTP and the `mcp:relay` OAuth scope. The public MCP entry is
`wendy-relay`. Its OAuth configuration declares the documented CIMD client form,
`client.mode: cimd`, with no client ID or credentials. Wendy resolves and
validates ChatGPT's client metadata document and requires PKCE. No client secret
is packaged. Empty
default scopes and disabled OIDC prevent unrelated login scopes from being added.
A signed-in, compatible CLI must run on a computer while ChatGPT uses
the plugin. Connect with the same Wendy account and organization, then ask
ChatGPT to check Wendy's status. That first tool call starts pairing. Compare
the returned code with the terminal code, accept it, and retry the status check.

```sh
wendy auth login
wendy cloud mcp --allow-read --allow-tool device_connect,camera_snapshot,container_stop
```

The example skips confirmation for built-in read-only device and Cloud tools
that do not access the host. It also permits the extra tools used by the review
walkthrough. Each connection, camera, or stop call still needs terminal
confirmation. By default, every permitted call asks for confirmation.
Local host access requires separate permission. `--allow-all` permits every
supported relay tool and skips all per-call confirmations. Calls stop when the
CLI exits. Keep its host awake and connected when using ChatGPT from a phone or
another computer.

Use `--auto-accept` to accept pairing requests from ChatGPT's known OAuth client
automatically. Tool permissions and per-call confirmations still apply. Other
clients require manual pairing. See [submission preparation](SUBMISSION.md)
for the relay tools, review cases, recording steps, and reviewer-access checks.

Package a review draft with
`python3 scripts/package-chatgpt-plugin.py --output /tmp/wendy-robots-marketplace-draft.zip`.

## Maintain the public and local skills

The public ZIP contains one independently authored skill at
`skills/wendy/SKILL.md`. It covers existing-device inspection, bounded telemetry,
requested camera snapshots, and starting or stopping named installed apps. It
requires an already installed CLI and leaves installation, builds, deployment,
firmware, and privileged setup to a separate local developer workflow.

Edit this public skill directly. Edit the full local developer skills in
`plugins/wendy-agentic-coding/skills`. Run `python3 scripts/sync-agent-skills.py`
from the repository root to embed both catalogs in the CLI. Synchronization does
not overwrite the public skill with the developer bundle. The packager rejects
extra public skill files so nested developer workflows cannot enter unnoticed.

The hosted relay serves the public skill through `skills/list`, `skills/get`,
and its listed `skill://wendy/wendy/` resource with a SHA-256 digest. The local
`wendy mcp serve` server retains the full developer catalog. A rebuilt CLI must
be running for an embedded-skill change to reach the relay.

OpenAI imports these files as a submission-time snapshot. After updating the
server, run Scan Tools again and review the imported skills before submitting a
new version. See [MCP skill import](https://developers.openai.com/plugins/build/mcp-server#import-skills-from-the-mcp-server)
and [plugin onboarding](https://github.com/openai/mcp-extensions/blob/main/docs/spec.md#plugin-onboarding).
