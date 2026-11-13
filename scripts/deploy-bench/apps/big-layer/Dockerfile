FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends build-essential \
    && rm -rf /var/lib/apt/lists/*
COPY seed /seed
RUN key=$(( $(cksum < /seed | cut -d' ' -f1) % 255 + 1 )) \
    && tar -C / -cf - usr | perl -e 'binmode STDIN; binmode STDOUT; my $k = chr(shift); local $/ = \1048576; while (my $buf = <STDIN>) { print $buf ^ ($k x length $buf) }' "$key" > /payload.bin
CMD ["sleep", "infinity"]
