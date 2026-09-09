FROM python:3.12-slim-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential libedit2 libnuma1 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir max==26.5.0 numpy==2.4.3
WORKDIR /app
COPY graph.py .
ENV MAX_DEVICE=cpu
CMD ["python", "-u", "graph.py"]
