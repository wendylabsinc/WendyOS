# Local Wendy submission preparation

The first proposed release runs `wendy mcp gateway` on the user's computer.
It includes the Wendy onboarding skill and icon. Hosted Cloud account linking
is separate work and is not needed for this local connection.

## Installation

Onboarding checks for the Wendy CLI, installs it when the user's local task
requires it, then runs:

```sh
wendy mcp setup chatgpt
```

The CLI writes a restricted policy at `~/.wendy/chatgpt/gateway.json`, exports
the local plugin, and adds its personal marketplace entry. The generated MCP
configuration uses absolute binary and policy paths. The user follows the
printed restart/install steps in ChatGPT Desktop. Default setup needs no
Cloud account or device and grants no simulator, project or host access.

The portable archive contains `mcp.json` with `wendy mcp gateway`. It requires
the CLI and the setup-generated connection before it can run. Installing the
ZIP alone does not install a binary or grant local access. Keep generated
policies, credentials, private paths and installed app bindings out of uploads.

## Export

Edit listing values in this directory's `plugin.json`, then run:

```sh
python3 scripts/package-chatgpt-plugin.py --output /tmp/wendy-robots-local-draft.zip
```

The exporter includes only `plugin.json`, `mcp.json`, `skills/`, and `assets/`.
It verifies icon paths and PNG dimensions, listing lengths, local MCP wiring,
and the finished ZIP inventory. It does not attest that public submission is
ready, verify remote pages, upload, or publish.

## Remaining preparation

- Confirm the local-MCP submission route with OpenAI. The current
  [package guidance](https://developers.openai.com/plugins/build/plugins#bundled-mcp-servers-and-lifecycle-hooks)
  requires a remote HTTPS endpoint for public MCP submission and directs local
  MCP authors to an OpenAI contact. A private tunnel is a development route.
- Complete developer identity verification for Wendy Labs Inc. On 2026-09-30,
  the submission portal blocked creation/upload before package validation
  because no verified developer identity was available.
- Supply `supportURL`, `privacyPolicyURL`, and `termsOfServiceURL` under
  `extensions.com.openai.interface`, and verify all four listing pages,
  including the existing `websiteURL`. Published policies must cover local
  project access, device/camera data, and any optional Cloud use actually offered.
- Confirm publisher identity, country availability and commerce behavior.
  Add release notes under `extensions.com.openai.publication.release_notes`.
- Prepare and run five positive and three negative cases against the release,
  record a real demo, and arrange reviewer access through the supported local
  review route. No cases or demo are claimed to be complete by this exporter.
- Have the publisher complete the portal's legal/policy attestations. Upload,
  submission for review and publication are separate steps.

See the [submission guide](https://developers.openai.com/plugins/deploy/submission).
This package is a local review draft, not an accepted public submission.
