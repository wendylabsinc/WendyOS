FROM debian:bookworm-slim
COPY extra-packages.txt /tmp/extra-packages.txt
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential $(cat /tmp/extra-packages.txt) \
    && rm -rf /var/lib/apt/lists/*
CMD ["sleep", "infinity"]
