FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends containerd dbus ca-certificates iproute2 iptables && rm -rf /var/lib/apt/lists/*
CMD ["sleep", "infinity"]
