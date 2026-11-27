# Build with the WendyOS repository root as the Docker context.
FROM golang:1.27.4 AS build
ARG WORLD_SIMULATION_BUILD_TAGS=""
WORKDIR /src
COPY go.mod go.sum ./
COPY go/babel/go.mod ./go/babel/go.mod
COPY go/proto/gen/sensorlinkpb ./go/proto/gen/sensorlinkpb
COPY go/world/internal ./go/world/internal
COPY go/world/command ./go/world/command
COPY go/world/gateway ./go/world/gateway
COPY go/world/cmd ./go/world/cmd
RUN CGO_ENABLED=0 go build -tags "$WORLD_SIMULATION_BUILD_TAGS" -trimpath -o /out/worldd ./go/world/cmd/worldd && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-ingest ./go/world/cmd/world-ingest && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-query ./go/world/cmd/world-query && \
    CGO_ENABLED=0 go build -trimpath -o /out/world ./go/world/cmd/world && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-mcp ./go/world/cmd/world-mcp && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-executor ./go/world/cmd/world-executor && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-sense ./go/world/cmd/world-sense && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-fuse ./go/world/cmd/world-fuse && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-evidence ./go/world/cmd/world-evidence && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-live-sense ./go/world/cmd/world-live-sense && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-record ./go/world/cmd/world-record && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-replay ./go/world/cmd/world-replay && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-sensorlink ./go/world/cmd/world-sensorlink && \
    CGO_ENABLED=0 go build -trimpath -o /out/world-sensorlink-sim ./go/world/cmd/world-sensorlink-sim

FROM scratch
COPY --from=build /out/ /
COPY go/world/third_party/sensorlink /notices/sensorlink
COPY go/world/third_party/go-mcp /notices/go-mcp
USER 65532:65532
ENTRYPOINT ["/worldd"]
