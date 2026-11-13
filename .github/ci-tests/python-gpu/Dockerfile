FROM dustynv/pytorch:2.7-r36.4.0-cu128-24.04
# Use the host driver injected by CDI. The base image puts its CUDA 12.8
# compatibility driver first, which fails with error 801 on JetPack 7.2.
# Keep the image's CUDA runtime libraries, but omit /usr/local/cuda/compat.
ENV LD_LIBRARY_PATH=/usr/local/cuda/lib64
WORKDIR /app
COPY main.py .
ENV PYTHONUNBUFFERED=1
CMD ["python3", "main.py"]
