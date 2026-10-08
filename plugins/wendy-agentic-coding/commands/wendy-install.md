---
description: Install or verify the Wendy CLI on macOS, Linux, or Windows.
argument-hint: [setup goal, OS, or failing command]
---

Use the `wendy-install` skill. Set up or verify Wendy CLI for:

`$ARGUMENTS`

Check whether `wendy --version` already works before running an installer. Use `curl -fsSL https://install.wendy.dev/cli.sh | bash` on macOS/Linux and `winget install WendyLabs.Wendy --source winget` on Windows. Verify the installed version, CLI help and actual MCP tool list. Restart the MCP host after updating the CLI. If the task is to install a board or robot, use `wendy-device-install` to choose OS flashing versus agent-only setup and verify first boot.
