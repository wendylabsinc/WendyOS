FROM python:3.12-slim-bookworm

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential libedit2 zlib1g \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir mojo==1.0.0

COPY main.mojo .
RUN mojo --version && mojo build main.mojo -o /app/hello

CMD ["/app/hello"]
