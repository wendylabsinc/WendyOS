FROM golang:1.26.6-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 iputils-ping tcpdump jq && rm -rf /var/lib/apt/lists/*
WORKDIR /module
