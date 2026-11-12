ARG GO_VERSION=1.27.4
FROM golang:${GO_VERSION}-bookworm
ARG GREMLINS_VERSION=v0.6.0
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-grpcio python3-protobuf protobuf-compiler tini \
    ca-certificates libasound2-dev libusb-1.0-0-dev \
    && rm -rf /var/lib/apt/lists/*
RUN GOBIN=/usr/local/bin go install github.com/go-gremlins/gremlins/cmd/gremlins@${GREMLINS_VERSION} \
    && go clean -modcache -cache
RUN useradd --uid 10001 --create-home worker && mkdir /state /work && chown worker:worker /state /work
COPY worker.py review.py config.json entrypoint.py bootstrap.py governor.py host_probe.py host_shell.proto /opt/worker/
COPY model/lease_runner.py /opt/worker/model/
RUN protoc -I/opt/worker --python_out=/opt/worker /opt/worker/host_shell.proto
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/state/home \
    GOCACHE=/state/cache/build GOMODCACHE=/state/cache/mod \
    WORK_ROOT=/work/source WORK_TMP=/work/tmp GOTOOLCHAIN=local
WORKDIR /state
ENTRYPOINT ["/usr/bin/tini", "--", "python3", "/opt/worker/entrypoint.py"]
