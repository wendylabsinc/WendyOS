ARG ALPINE_IMAGE=alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1
FROM ${ALPINE_IMAGE}

RUN apk add --no-cache \
        buildkit \
        ca-certificates \
        cni-plugins \
        containerd \
        e2fsprogs \
        iproute2 \
        iptables \
        linux-virt \
        nerdctl \
        runc \
        util-linux \
    && apk info -vv | sort > /usr/share/wendy-runtime-packages.txt

COPY rootfs/ /
COPY wendy-runtime-guest-proxy /usr/local/libexec/wendy-runtime-guest-proxy

RUN chmod 0755 /init /usr/local/libexec/wendy-runtime-guest-proxy \
    && mkdir -p /etc/buildkit /etc/containerd /etc/cni/net.d \
        /run/buildkit /run/containerd /var/lib/wendy-runtime

ENTRYPOINT ["/init"]
