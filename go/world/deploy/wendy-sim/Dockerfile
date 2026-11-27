# The generated build context contains only these binaries and public seed.
# No repository checkout, lab clients, root private key or device key is copied.
FROM scratch
LABEL org.opencontainers.image.title="Wendy World simulation observation node"
COPY worldd /usr/local/bin/worldd
COPY world-wendy-boot /usr/local/bin/world-wendy-boot
COPY node-seed.json /etc/wendy-world/node-seed.json
USER 0:0
ENTRYPOINT ["/usr/local/bin/world-wendy-boot"]
