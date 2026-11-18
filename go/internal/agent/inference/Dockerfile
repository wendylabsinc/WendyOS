# The Python environment of the agent's campaign model worker, for running it
# in a container. The image holds only the worker's locked dependencies: the
# agent mounts its own worker.py and accelerator.py read-only, so the worker
# itself can never differ from the agent that drives it.
#
# Build with this directory as the context:
#   docker build --build-arg DEPS_HASH="$(cat pyproject.toml uv.lock | sha256sum | cut -c1-16)" .
FROM python:3.12-slim-bookworm@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df AS base

FROM base AS build
# The uv the agent installs on devices (uvVersion in runtime.go).
COPY --from=ghcr.io/astral-sh/uv:0.10.9@sha256:10902f58a1606787602f303954cea099626a4adb02acbac4c69920fe9d278f82 /uv /usr/local/bin/uv
WORKDIR /opt/wendy-inference
COPY pyproject.toml uv.lock ./
# The agent starts the worker only in an image labelled with its own
# inference.DepsHash, so an image is never labelled for another lock.
ARG DEPS_HASH
RUN test "$(cat pyproject.toml uv.lock | sha256sum | cut -c1-16)" = "$DEPS_HASH"
# The same locked, wheels-only environment the agent builds on a device.
RUN UV_PROJECT_ENVIRONMENT=/opt/wendy-inference/venv UV_PYTHON_DOWNLOADS=never \
    uv sync --frozen --no-dev --no-build --no-install-project --no-cache --compile-bytecode --python /usr/local/bin/python3

FROM base
COPY --from=build /opt/wendy-inference/venv /opt/wendy-inference/venv
ARG DEPS_HASH
LABEL org.opencontainers.image.source="https://github.com/wendylabsinc/WendyOS" \
      sh.wendy.inference.deps="$DEPS_HASH"
