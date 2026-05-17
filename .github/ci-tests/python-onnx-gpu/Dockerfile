FROM ubuntu:22.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pip python3-dev libopenblas-dev \
    && rm -rf /var/lib/apt/lists/*

# Install onnxruntime-gpu from Jetson AI Lab (CUDA 12.6 for JetPack 6.x).
# CDI maps all CUDA libraries from the host at runtime.
RUN pip3 install --no-cache-dir onnxruntime-gpu \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

# Install onnx (model creation) and numpy separately from PyPI.
RUN pip3 install --no-cache-dir onnx numpy==1.26.4

RUN pip3 show onnxruntime-gpu | grep Version && echo "onnxruntime-gpu installed"

WORKDIR /app
COPY main.py .
ENV PYTHONUNBUFFERED=1
CMD ["python3", "main.py"]
