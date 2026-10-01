# Get started with Wendy

This skills-only plugin helps users begin before they have the CLI, hardware,
or a Wendy Cloud account. It explains Wendy in one sentence, starts with the
user's goal, and guides installation only when their chosen workflow needs it.
It has no MCP dependency, installation hook, or automatic download.

Install this package through the supported plugin authoring or marketplace
flow. Start a conversation with "Help me get started. I have not used Wendy
before." The skill can guide setup without a connected tool. A local terminal
is needed to execute setup on the user's machine; in a web-only chat it gives
the user the appropriate commands.

For desktop use, install the CLI and run `wendy mcp setup chatgpt`. For a hosted
connection, use the existing registered Wendy plugin and its account connection
flow. Hosted use does not require the CLI. Neither installing this plugin nor
opening a setup conversation erases storage or creates a simulator.

The shared workflows are generated from `plugins/wendy-agentic-coding/skills`.
Run `python3 scripts/sync-agent-skills.py` after editing those sources. Public
directory availability depends on submission and review; this source package
does not register or publish an OpenAI plugin.
