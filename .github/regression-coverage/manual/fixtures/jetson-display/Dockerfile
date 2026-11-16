FROM ubuntu:24.04 AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev libegl-dev libgles-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY main.c .
RUN gcc -std=c11 -Wall -Wextra -Werror main.c -o /display-probe -lEGL -lGLESv2

FROM ubuntu:24.04
# GLVND dispatch comes from the image; the host NVIDIA implementation must
# arrive through display entitlement CDI, not a bundled NVIDIA driver.
RUN apt-get update && apt-get install -y --no-install-recommends libegl1 libgles2 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /display-probe /usr/local/bin/display-probe
CMD ["/usr/local/bin/display-probe"]
