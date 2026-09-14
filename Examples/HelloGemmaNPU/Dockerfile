# glibc >= 2.38 is the floor for the Qualcomm FastRPC/QNN userspace.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 libyaml-0-2 libc++1 libc++abi1 libatomic1 libstdc++6 \
 && rm -rf /var/lib/apt/lists/*
COPY genie-htp.tgz /
RUN tar xzf /genie-htp.tgz -C / && rm /genie-htp.tgz \
 && ln -sf libcdsprpc.so.1.0.0 /usr/lib/libcdsprpc.so.1 \
 && ln -sf libcdsprpc.so.1.0.0 /usr/lib/libcdsprpc.so \
 && ln -sf libadsprpc.so.1.0.0 /usr/lib/libadsprpc.so.1 \
 && ln -sf libadsprpc.so.1.0.0 /usr/lib/libadsprpc.so \
 && ldconfig
WORKDIR /app
COPY app.py genie_config.json htp_backend_ext_config.json ./
COPY model ./model
EXPOSE 8080
CMD ["python3", "-u", "app.py"]
