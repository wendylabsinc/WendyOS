# Build with the same pinned Go toolchain image as WendyOS (crypto/mldsa).
ARG GO_BUILD_IMAGE
FROM ${GO_BUILD_IMAGE} AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/wendy-cloud-mcp ./go/cmd/wendy-cloud-mcp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/wendy-cloud-mcp /wendy-cloud-mcp
USER 65532:65532
ENTRYPOINT ["/wendy-cloud-mcp"]
CMD ["--config", "/run/wendy-cloud-mcp/config.json"]
