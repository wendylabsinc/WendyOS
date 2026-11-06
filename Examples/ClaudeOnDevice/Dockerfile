# Claude-on-device runs the Claude Code CLI inside an admin-entitled container
# so the device can operate and debug itself over the local agent socket.
#
# node:22.23.4-bookworm-slim is multi-arch; built for arm64 (the Jetson) via buildx.
FROM node:22.23.4-bookworm-slim

# git + ripgrep are used by Claude Code; ca-certificates for the Anthropic API;
# curl is used below to download the BuildKit release tarball.
RUN apt-get update \
  && apt-get install -y --no-install-recommends git ca-certificates ripgrep less curl \
  && rm -rf /var/lib/apt/lists/*

# BuildKit (buildkitd + buildctl + buildkit-runc) for on-device image builds —
# no Docker daemon required. Pinned release, arm64. The tarball SHA-256 is
# verified before extraction: buildkitd runs with the privileged `build`
# capability set, so a tampered binary (compromised release asset, CDN, or a
# MITM behind a rogue CA) would have immediate host-escape reach. Bump
# BUILDKIT_SHA256 alongside BUILDKIT_VERSION (it is the sha256 of the
# linux-arm64 release tarball).
ARG BUILDKIT_VERSION=v0.16.0
ARG BUILDKIT_SHA256=1b70bb7955ddda66537d4bf9aa540e79e79e19aa989901613da58f5f133a53ef
RUN curl -fsSL -o /tmp/buildkit.tar.gz \
      "https://github.com/moby/buildkit/releases/download/${BUILDKIT_VERSION}/buildkit-${BUILDKIT_VERSION}.linux-arm64.tar.gz" \
  && echo "${BUILDKIT_SHA256}  /tmp/buildkit.tar.gz" | sha256sum -c - \
  && tar -xz -C /usr/local -f /tmp/buildkit.tar.gz bin/buildkitd bin/buildctl bin/buildkit-runc \
  && rm /tmp/buildkit.tar.gz \
  && test -x /usr/local/bin/buildctl && test -x /usr/local/bin/buildkitd
# NOTE: do NOT run `buildkitd --version` here — when this image is cross-built for
# arm64 on an amd64 host, that executes the arm64 daemon under QEMU and can abort
# the build, leaving the device on a stale (buildkit-less) image. A file check is
# enough; the daemon is exercised at runtime by init.sh.
# buildctl reaches the in-container daemon over this socket (init.sh starts it).
ENV BUILDKIT_HOST=unix:///run/buildkit/buildkitd.sock

# Claude Code CLI.
RUN --mount=type=cache,target=/root/.npm npm install -g @anthropic-ai/claude-code

# The wendy CLI built from the claude-on-device branch (understands
# WENDY_AGENT_SOCKET). Stage the arm64 binary into the build context as
# `wendy-linux-arm64` before building — see README.md.
COPY wendy-linux-arm64 /usr/local/bin/wendy
RUN chmod +x /usr/local/bin/wendy

# First-run init: registers the Wendy MCP server with Claude Code on every start.
COPY init.sh /usr/local/bin/claude-on-device-init
RUN chmod +x /usr/local/bin/claude-on-device-init

# Runs as root (the default) on purpose: the admin entitlement bind-mounts the
# agent's root-owned 0660 control socket, so the process must be root to use it.
# /root (home: OAuth token, config, MCP setup) and /workspace are persisted via
# the `persist` entitlements in wendy.json.
WORKDIR /workspace

# init.sh runs `wendy mcp setup` then idles (sleep infinity) so the container
# stays up; `wendy device attach claude-on-device` execs `claude` with a PTY.
CMD ["/usr/local/bin/claude-on-device-init"]
