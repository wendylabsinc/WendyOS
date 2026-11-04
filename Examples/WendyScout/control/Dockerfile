FROM docker.io/ollama/ollama:0.40.14@sha256:a2be4eed855ab2df84585f25e608fa68088988d2470c14dc736166b4770946c5
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-grpcio python3-protobuf protobuf-compiler aria2 curl ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/scout
COPY *.py pins.json canonical.json ./
RUN python3 generate.py
ENV LD_LIBRARY_PATH=/usr/lib/ollama:/usr/lib/ollama/cuda_v13:/usr/local/nvidia/lib:/usr/local/nvidia/lib64
ENV GGML_BACKEND_PATH=/usr/lib/ollama/cuda_v13/libggml-cuda.so
ENTRYPOINT ["python3", "-u", "/opt/scout/main.py"]
