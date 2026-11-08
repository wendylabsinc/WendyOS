FROM debian:bookworm-slim AS vision
RUN apt-get update && apt-get install -y --no-install-recommends g++ cmake ninja-build curl ca-certificates zlib1g-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go/internal/twinvision/native/opencv.lock go/internal/twinvision/native/build-linux.sh ./
RUN sh build-linux.sh
COPY go/internal/twinvision/native/ .
RUN cmake -S . -B /build -G Ninja -DCMAKE_BUILD_TYPE=Release -DOpenCV_DIR=/opt/opencv/lib/cmake/opencv4 && cmake --build /build --parallel 8 && ctest --test-dir /build --output-on-failure && cmake --install /build
RUN mkdir -p /runtime/usr/local/bin && cp /usr/local/bin/wendy-twin-vision /runtime/usr/local/bin/ && ldd /usr/local/bin/wendy-twin-vision | awk '/=> \/|^\s*\// {for (i=1;i<=NF;i++) if ($i ~ /^\//) print $i}' | xargs -I '{}' cp -L --parents '{}' /runtime
RUN mkdir -p /runtime/usr/share/doc/wendy-twin-vision && cp /tmp/opencv-*/LICENSE /runtime/usr/share/doc/wendy-twin-vision/OpenCV-LICENSE.txt && cp /opt/opencv/share/licenses/opencv4/* /runtime/usr/share/doc/wendy-twin-vision/
RUN for loader in /lib/ld-linux-aarch64.so.1 /lib64/ld-linux-x86-64.so.2; do if [ -e "$loader" ]; then cp -L --parents "$loader" /runtime; fi; done

# Build with the repository root as context.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY go ./go
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux go build -trimpath -o /wendy-twin ./go/cmd/wendy-twin
FROM gcr.io/distroless/static-debian12
COPY --from=vision /runtime/ /
COPY --from=build /wendy-twin /usr/local/bin/wendy-twin
ENV WENDY_TWIN_DATA=/data
ENTRYPOINT ["/usr/local/bin/wendy-twin"]
