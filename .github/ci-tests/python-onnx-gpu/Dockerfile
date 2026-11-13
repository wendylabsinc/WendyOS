FROM ubuntu:22.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pip python3-dev libopenblas-dev \
    && rm -rf /var/lib/apt/lists/*

# Pin the CUDA 12.6 Jetson wheel containing Orin (sm_87) kernels. Ordinary
# Python dependencies still resolve from PyPI, without allowing a newer
# onnxruntime-gpu release there to replace this CUDA-12 build.
RUN pip3 install --no-cache-dir \
    'https://pypi.jetson-ai-lab.io/jp6/cu126/+f/d98/0b934b9a29c1a/onnxruntime_gpu-1.24.0-cp310-cp310-linux_aarch64.whl#sha256=d980b934b9a29c1a9d6f39751edd7662b69fadd75556a10ff363773a58ce0950'

# CDI supplies the host driver, but JetPack 7 hosts do not supply the CUDA 12
# runtime this wheel needs. Keep the runtime in the image and ahead of host
# libraries with overlapping sonames, such as libcurand.so.10 and libcudnn.so.9.
RUN pip3 install --no-cache-dir --target /opt/cuda12/python \
        nvidia-cuda-runtime-cu12 nvidia-cuda-nvrtc-cu12 nvidia-cublas-cu12 \
        nvidia-cudnn-cu12 nvidia-curand-cu12 nvidia-cufft-cu12 nvidia-nvjitlink-cu12 \
    && mkdir -p /opt/cuda12/lib \
    && find /opt/cuda12/python -name '*.so*' -exec ln -s '{}' /opt/cuda12/lib/ ';' \
    && printf '%s\n' /opt/cuda12/lib > /etc/ld.so.conf.d/000-cuda12.conf \
    && ldconfig
ENV LD_LIBRARY_PATH=/opt/cuda12/lib

# Install onnx (model creation) and numpy separately from PyPI.
RUN pip3 install --no-cache-dir onnx numpy==1.26.4

RUN pip3 show onnxruntime-gpu | grep Version && echo "onnxruntime-gpu installed"

WORKDIR /app
COPY main.py .
ENV PYTHONUNBUFFERED=1
CMD ["python3", "main.py"]
