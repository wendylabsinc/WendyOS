FROM python:3.12-slim-bookworm

WORKDIR /app
ENV PYTHONUNBUFFERED=1

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential libedit2 zlib1g \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir mojo==1.0.0

COPY main.mojo http_bridge.py ./
RUN mojo --version && mojo build main.mojo -o /app/server

EXPOSE 8080
CMD ["/app/server"]
