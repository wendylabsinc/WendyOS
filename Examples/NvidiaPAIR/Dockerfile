# syntax=docker/dockerfile:1.6
ARG OLLAMA_IMAGE=ollama/ollama:0.40.13@sha256:b2f0122c476bc9fe7ce679d8bb4bd555351e25e2554eefb156f61cb0a89cf429
# Build the complete headless bundle from the PAIR 0.1.1 release commit.
FROM --platform=$BUILDPLATFORM golang:1.26.5 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG PAIR_REVISION=13b68115fa2c9c1d94f1ead1358f8d5a527cfecf
RUN apt-get update && apt-get install -y --no-install-recommends jq \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git init . \
    && git remote add origin https://github.com/NVIDIA/Personal-AI-Router.git \
    && git fetch --depth 1 origin "${PAIR_REVISION}" \
    && git checkout --detach FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = "${PAIR_REVISION}"
COPY patches/ /patches/
RUN git apply --check /patches/wendy-mesh-addresses.patch \
    && git apply /patches/wendy-mesh-addresses.patch
# PAIR owns the packaged engine instead of installing executables into /data.
# Preserve the host-injected library path for NVIDIA CDI.
RUN jq '.platforms["linux/arm64"].detect = ["/usr/bin/ollama"] | \
        .platforms["linux/amd64"].detect = ["/usr/bin/ollama"] | \
        .platforms["linux/arm64"].runtime = {"bin":"/usr/bin/ollama"} | \
        .platforms["linux/amd64"].runtime = {"bin":"/usr/bin/ollama"}' \
        services/nvpair-engine-manager/manifests/ollama.json > /tmp/ollama.json \
    && mv /tmp/ollama.json services/nvpair-engine-manager/manifests/ollama.json
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd services/shared && go test ./netpick \
    && cd ../nvpair-cluster-manager && go test ./...
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} bash services/build.sh

# Download on the build host, before the app enters its mesh-only network.
FROM --platform=$BUILDPLATFORM ${OLLAMA_IMAGE} AS model
ARG OLLAMA_MODEL=qwen2.5:0.5b
# Ollama 0.40.13 stores an extra layer referencing the original registry manifest.
# Pin the resulting local manifest, including that provenance layer.
ARG MODEL_SHA256=6467b5fd1dd41b31569d53f7286b0db4250515449471171370b38a96ba2216fc
ENV OLLAMA_MODELS=/opt/ollama-models OLLAMA_HOST=127.0.0.1:11434
RUN set -eu; \
    ollama serve > /tmp/ollama.log 2>&1 & server=$!; \
    trap 'kill "$server"; wait "$server" || true' EXIT; \
    ready=0; for attempt in $(seq 1 60); do \
      if ollama list >/dev/null 2>&1; then ready=1; break; fi; sleep 1; \
    done; test "$ready" = 1; \
    ollama pull "$OLLAMA_MODEL"; \
    printf '%s  %s\n' "$MODEL_SHA256" \
      "/opt/ollama-models/manifests/registry.ollama.ai/library/${OLLAMA_MODEL%:*}/${OLLAMA_MODEL##*:}" \
      | sha256sum -c -; \
    printf '%s\n' "$OLLAMA_MODEL" > /opt/ollama-models/example-model

FROM ${OLLAMA_IMAGE}
# PAIR runs the packaged Ollama. No desktop or systemd is needed.
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 ca-certificates curl procps pciutils \
    && rm -rf /var/lib/apt/lists/*
ENV HOME=/data \
    XDG_CONFIG_HOME=/data/.config \
    OLLAMA_HOST=127.0.0.1:11434 \
    PAIR_PORT=11434 \
    ENABLE_OLLAMA=true \
    OLLAMA_MODELS=/opt/ollama-models \
    OLLAMA_KEEP_ALIVE=-1 \
    OLLAMA_NUM_PARALLEL=1 \
    OLLAMA_CONTEXT_LENGTH=2048 \
    OLLAMA_LOAD_TIMEOUT=10m \
    PYTHONUNBUFFERED=1
COPY --from=builder /src/services/build/bin/ /opt/nvpair/
COPY --from=builder /src/LICENSE /src/THIRD_PARTY_NOTICES.md /opt/nvpair/licenses/
COPY --from=model /opt/ollama-models/ /opt/ollama-models/
COPY pair.py fleet_check.py /opt/wendy/
RUN mkdir -p /data /run/nvpair \
    && ln -s /opt/wendy/pair.py /usr/local/bin/pairctl \
    && chmod 0755 /opt/wendy/pair.py
WORKDIR /opt/nvpair
HEALTHCHECK --interval=30s --timeout=10s --start-period=300s \
    CMD ["pairctl", "health"]
ENTRYPOINT ["python3", "/opt/wendy/pair.py", "serve"]
