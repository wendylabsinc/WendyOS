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

# Install Python packages via pip for better version control
# numpy, scipy, matplotlib from pip (not apt) to avoid conflicts
RUN pip3 install --no-cache-dir \
    numpy \
    scipy \
    matplotlib

# Install PyTorch from NVIDIA's Jetson-optimized wheels
# These are pre-compiled for ARM64 and optimized for Jetson with CUDA 12.6
# This leverages CDI-mapped CUDA/cuDNN from the host system
RUN pip3 install --no-cache-dir \
    torch torchvision torchaudio \
    --index-url https://pypi.jetson-ai-lab.io/jp6/cu126/

# Copy application code
COPY gpu-test.py /app/
WORKDIR /app

# Create a non-root user
RUN useradd --create-home --shell /bin/bash app && \
    chown -R app:app /app
USER app

# Set environment variables
ENV PYTHONUNBUFFERED=1

# Default command - run comprehensive GPU test
CMD ["python3", "gpu-test.py"]
