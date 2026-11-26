# Screencast Tooling

The `screencast/` directory is developer-only repository tooling for producing
narrated engineering screencasts. It is not part of WendyOS, the Wendy CLI, the
device runtime, or public product documentation.

See `screencast/README.md` in the repository for the source of truth:
authoring workflow, local commands, CI checks, voiceover setup, generated output
rules, and hook safety.

The `Screencast` GitHub Actions workflow (`.github/workflows/screencast.yml`)
runs on pull requests to `main` that touch `screencast/**`, the workflow file, or
Renovate configuration.
