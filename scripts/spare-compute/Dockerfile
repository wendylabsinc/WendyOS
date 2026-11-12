# Match the repository's Go toolchain. Build natively for amd64 or arm64.
ARG GO_VERSION=1.27.4
FROM golang:${GO_VERSION}-bookworm
ARG GREMLINS_VERSION=v0.6.0
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 ca-certificates libasound2-dev libusb-1.0-0-dev \
    && rm -rf /var/lib/apt/lists/*
RUN GOBIN=/usr/local/bin go install github.com/go-gremlins/gremlins/cmd/gremlins@${GREMLINS_VERSION} \
    && go clean -modcache -cache
RUN useradd --uid 10001 --create-home worker && mkdir /state && chown worker:worker /state
COPY worker.py review.py config.json /opt/worker/
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/state/home \
    GOCACHE=/state/cache/build GOMODCACHE=/state/cache/mod \
    GOTMPDIR=/state/tmp TMPDIR=/state/tmp GOTOOLCHAIN=local
USER 10001:10001
WORKDIR /state
ENTRYPOINT ["python3", "/opt/worker/worker.py"]
