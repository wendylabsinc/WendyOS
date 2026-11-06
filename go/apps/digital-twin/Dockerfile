# Build with the repository root as context.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY go ./go
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux go build -trimpath -o /wendy-twin ./go/cmd/wendy-twin
FROM gcr.io/distroless/static-debian12
COPY --from=build /wendy-twin /usr/local/bin/wendy-twin
ENV WENDY_TWIN_DATA=/data
ENTRYPOINT ["/usr/local/bin/wendy-twin"]
