# Simulation node packaging

Generate a private staging directory with `go run ./cmd/world-wendy-lab stage`.
See [the deployment runbook](../../docs/wendy-vm-deployment.md). This directory
contains image templates, not a ready-to-deploy app or credentials.

The generator copies only static Linux binaries and a public verification seed
into the Docker context. Client signing keys remain outside that directory.
The node starts as root to initialize its unique persistent volume, then drops
to UID/GID 65532 before running World. Executables remain outside `/data`.

This package enables signed simulation observation publication and reads only.
It does not configure tasks, robot execution, physical authority, or evidence
capture. The initial VM transport is an explicitly selected HTTP fallback.
