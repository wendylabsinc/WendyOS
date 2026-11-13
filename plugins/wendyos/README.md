# Wendy skills-only plugin

This public-submission package contains one Wendy skill and all 12 end-user
workflows as supporting references. It has no MCP server, app binding, lifecycle
hook, or automatic installation. It provides guidance; device access and command
execution depend on separately available tools and user authorization.

The workflows come from `plugins/wendy-agentic-coding/skills`. Regenerate them with
`python3 scripts/sync-agent-skills.py`, then verify with `--check`.

Export the package with:

```sh
python3 scripts/package-wendyos-skills.py --output /tmp/wendyos-skills-0.1.0.zip
```

Public release requires the publisher's verified identity, listing pages, country
targeting, commerce declaration, portal attestations, and review approval. Archive
creation does not upload, submit, or publish the plugin. Skills-only submissions
do not require MCP review cases, a demo recording, or reviewer credentials.
