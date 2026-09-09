FROM python:3.12-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential libedit2 libnuma1 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir "max[serve]==26.5.0"
RUN python -c 'from huggingface_hub import snapshot_download; snapshot_download(repo_id="sentence-transformers/all-MiniLM-L6-v2", revision="1110a243fdf4706b3f48f1d95db1a4f5529b4d41", local_dir="/opt/models/all-MiniLM-L6-v2", allow_patterns=["*.json", "*.txt", "model.safetensors"])'

WORKDIR /app
COPY serve.sh .
ENV MAX_DEVICE=cpu HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 \
    MAX_SERVE_DISABLE_TELEMETRY=true
EXPOSE 8000
CMD ["sh", "serve.sh"]
