FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY go/modelhost/fakehost ./go/modelhost/fakehost
RUN go test ./go/modelhost/fakehost/... && CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -o /out/fakehost ./go/modelhost/fakehost

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fakehost /usr/local/bin/fakehost
ENTRYPOINT ["/usr/local/bin/fakehost"]
