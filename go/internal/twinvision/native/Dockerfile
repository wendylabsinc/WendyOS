FROM debian:bookworm-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends g++ cmake ninja-build curl ca-certificates zlib1g-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY opencv.lock build-linux.sh ./
RUN sh build-linux.sh
COPY . .
RUN cmake -S . -B /build -G Ninja -DCMAKE_BUILD_TYPE=Release -DOpenCV_DIR=/opt/opencv/lib/cmake/opencv4 && cmake --build /build --parallel 8 && ctest --test-dir /build --output-on-failure && cmake --install /build
RUN mkdir -p /runtime/usr/local/bin && cp /usr/local/bin/wendy-twin-vision /runtime/usr/local/bin/ && ldd /usr/local/bin/wendy-twin-vision | awk '/=> \/|^\s*\// {for (i=1;i<=NF;i++) if ($i ~ /^\//) print $i}' | xargs -I '{}' cp -L --parents '{}' /runtime
RUN mkdir -p /runtime/usr/share/doc/wendy-twin-vision && cp /tmp/opencv-*/LICENSE /runtime/usr/share/doc/wendy-twin-vision/OpenCV-LICENSE.txt && cp /opt/opencv/share/licenses/opencv4/* /runtime/usr/share/doc/wendy-twin-vision/
RUN for loader in /lib/ld-linux-aarch64.so.1 /lib64/ld-linux-x86-64.so.2; do if [ -e "$loader" ]; then cp -L --parents "$loader" /runtime; fi; done
FROM scratch
COPY --from=build /runtime/ /
