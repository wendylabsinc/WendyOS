# Runs the agent-experience benchmark on the Jetson it tests, with a local model on the GPU.
# Stage the build inputs first: sh evals/agent-experience/stage-on-device.sh (from the repo root).
# Build context: evals/agent-experience.
FROM docker.io/ollama/ollama:0.40.14@sha256:a2be4eed855ab2df84585f25e608fa68088988d2470c14dc736166b4770946c5

# procps gives the init script `free` and `pkill`; aria2 downloads the model and resumes.
RUN apt-get update \
  && apt-get install -y --no-install-recommends python3 git curl aria2 ca-certificates procps libusb-1.0-0 \
  && rm -rf /var/lib/apt/lists/*

# BuildKit (buildkitd + buildctl + buildkit-runc) for on-device builds. The tarball SHA-256 is checked
# before extraction because buildkitd runs with the privileged `build` entitlement.
ARG BUILDKIT_VERSION=v0.16.0
ARG BUILDKIT_SHA256=1b70bb7955ddda66537d4bf9aa540e79e79e19aa989901613da58f5f133a53ef
RUN curl -fsSL -o /tmp/buildkit.tar.gz \
      "https://github.com/moby/buildkit/releases/download/${BUILDKIT_VERSION}/buildkit-${BUILDKIT_VERSION}.linux-arm64.tar.gz" \
  && echo "${BUILDKIT_SHA256}  /tmp/buildkit.tar.gz" | sha256sum -c - \
  && tar -xz -C /usr/local -f /tmp/buildkit.tar.gz bin/buildkitd bin/buildctl bin/buildkit-runc \
  && rm /tmp/buildkit.tar.gz \
  && test -x /usr/local/bin/buildctl && test -x /usr/local/bin/buildkitd
# Do not run `buildkitd --version` here: a cross-built arm64 daemon runs under QEMU and can abort the build.

# The wendy CLI built from this checkout (it has the chat hook), and the skills it is tested with.
COPY .stage/wendy-linux-arm64 /usr/local/bin/wendy
RUN chmod +x /usr/local/bin/wendy
COPY .stage/treatment /opt/treatment
COPY . /opt/harness

ENV BUILDKIT_HOST=unix:///run/buildkit/buildkitd.sock \
    WENDY_ANALYTICS=false \
    LD_LIBRARY_PATH=/usr/lib/ollama:/usr/lib/ollama/cuda_v13:/usr/local/nvidia/lib:/usr/local/nvidia/lib64 \
    GGML_BACKEND_PATH=/usr/lib/ollama/cuda_v13/libggml-cuda.so \
    GGML_CUDA_ENABLE_UNIFIED_MEMORY=1 \
    GGML_CUDA_DISABLE_GRAPHS=1

# Root on purpose: the admin entitlement's agent socket is root-owned.
WORKDIR /opt/harness
RUN chmod +x /opt/harness/on-device-init.sh
ENTRYPOINT ["/opt/harness/on-device-init.sh"]
