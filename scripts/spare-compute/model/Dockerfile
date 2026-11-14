FROM vllm/vllm-openai:v0.30.0
USER root
RUN apt-get update && apt-get install -y --no-install-recommends tini \
    && rm -rf /var/lib/apt/lists/*
COPY lease_runner.py /opt/spare/lease_runner.py
ENV HOME=/cache HF_HOME=/cache/huggingface PYTHONUNBUFFERED=1 \
    LANE=gpu WORKLOAD_STATE=/cache XDG_CACHE_HOME=/work/cache \
    TRITON_CACHE_DIR=/work/triton VLLM_CACHE_ROOT=/work/vllm \
    TORCHINDUCTOR_CACHE_DIR=/work/torchinductor CUDA_CACHE_PATH=/work/cuda
# The monitor starts vLLM only while a fresh GPU permission exists.
# vLLM and its child processes run as UID 10001, not the monitor's root UID.
ENTRYPOINT ["/usr/bin/tini", "--", "python3", "/opt/spare/lease_runner.py"]
