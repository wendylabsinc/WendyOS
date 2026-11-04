ARG OLLAMA_IMAGE=ollama/ollama:latest
FROM debian:bookworm-slim AS cli
ARG WENDY_VERSION=2026.11.03-074612
ARG TARGETARCH=arm64
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY install-cli.sh /tmp/install-cli.sh
RUN sh /tmp/install-cli.sh

# Adapted from templates/python/llm/ollama, including JetPack 6 selection.
FROM ${OLLAMA_IMAGE} AS runtime
ARG WENDY_JETPACK_MAJOR=0
ARG WENDY_VERSION
ENV WENDY_TEST_VERSION=${WENDY_VERSION} \
    WENDY_JETPACK_MAJOR=${WENDY_JETPACK_MAJOR} \
    OLLAMA_HOST=127.0.0.1:11439 \
    OLLAMA_MODELS=/data/models \
    OLLAMA_MODEL=qwen3.8:27b \
    OLLAMA_LOAD_TIMEOUT=20m \
    PYTHONUNBUFFERED=1
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 bash ca-certificates curl procps iproute2 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=cli /usr/local/bin/wendy /usr/local/bin/wendy
COPY sh_wendy_regression /app/sh_wendy_regression
WORKDIR /app
ENTRYPOINT ["python3", "-m", "sh_wendy_regression"]
