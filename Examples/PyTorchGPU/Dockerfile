FROM ubuntu:22.04

# Install system dependencies and native libraries
# Using apt for system libraries, pip for Python packages to avoid conflicts
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3-pip \
    python3-dev \
    libopenblas-dev \
    libblas-dev \
    liblapack-dev \
    gfortran \
    cmake \
    build-essential \
    libgomp1 \
    && rm -rf /var/lib/apt/lists/*

# Install PyTorch from NVIDIA's Jetson-optimized wheels FIRST
# These are pre-compiled for ARM64 and optimized for Jetson with CUDA 12.6
# This leverages CDI-mapped CUDA/cuDNN from the host system
# Pin specific versions to ensure we get the CUDA-enabled builds
# NOTE: Jetson AI Lab PyTorch may not depend on numpy or vendors it internally
RUN pip3 install --no-cache-dir \
    torch==2.8.0 \
    torchvision==0.23.0 \
    torchaudio==2.8.0 \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/


# Install scientific Python packages with pinned numpy version
RUN pip3 install --no-cache-dir \
    numpy==1.26.4 \
    scipy \
    matplotlib
# Verify we got the correct PyTorch version (will fail build if wrong)
# Use pip show instead of import to avoid CUDA library loading during build
RUN pip3 show torch | grep "Version: 2.8.0" && echo "✓ Correct PyTorch version installed"

# Set working directory and copy application code
WORKDIR /app
COPY gpu-test.py .

# Create a non-root user
# NOTE: Commented out for now - CUDA memory manager needs root access to /dev/nvmap
# RUN useradd --create-home --shell /bin/bash app && \
#     chown -R app:app /app
# USER app

# Set environment variables
ENV PYTHONUNBUFFERED=1

# Default command - run bash to keep container alive
# CMD ["bash", "-c", "while true; do sleep 1000; done"]
CMD ["python3", "gpu-test.py"]

