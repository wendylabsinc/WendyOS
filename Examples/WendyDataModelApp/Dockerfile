# syntax=docker/dockerfile:1.6
#
# A Dockerfile rather than a Stagefile because the model is produced by a
# build step (an Ultralytics ONNX export) that the Stagefile DSL has no
# instruction for. The export stage is build-time only: ultralytics and
# torch never enter the runtime image, which carries onnxruntime, OpenCV,
# NumPy, grpcio, and PyAV on the CPU (see the runtime stage below).
#
# Jetson GPU variant: see README.md. The runtime stage swaps
# onnxruntime for the onnxruntime-gpu wheel from the Jetson wheel index
# (the same wheel HelloONNX's Stagefile resolves with `cuda: true`).

FROM python:3.11-slim-bookworm AS export
# Ultralytics depends on the GUI opencv-python wheel, whose import needs
# X client libraries the slim image lacks (libxcb, libGL, glib). They are
# build-stage only; the runtime stage keeps opencv-python-headless.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libxcb1 libgl1 libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*
# Pin the Ultralytics release; it is the model version reported in every
# prediction record (WENDY_MODEL_VERSION) and pinned in campaign.yaml.
# onnxscript backs torch.onnx.export from torch 2.13 onwards.
RUN pip install --no-cache-dir "ultralytics==8.3.63" "onnx<2" onnxruntime onnxscript
WORKDIR /export
# Downloads the pinned yolov8n.pt weights and exports yolov8n.onnx.
# opset 12 keeps the graph loadable by every onnxruntime this app targets.
RUN yolo export model=yolov8n.pt format=onnx imgsz=640 opset=12


FROM python:3.11-slim-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends libglib2.0-0 \
    && rm -rf /var/lib/apt/lists/*
# av decodes the encoded samples the harness delivers (H.264 access units on
# the native Video4Linux2 path, byte-stream chunks elsewhere); grpcio carries
# the sensor subscription. Neither needs a camera device node.
#
# av is pinned exactly rather than bounded: PyAV 14.3.0 and 14.4.0 ship no
# manylinux aarch64 wheel, so a bare "av<15" resolves to 14.4.0 on this
# Jetson's arm64 runtime and falls back to a source build, which wants
# ffmpeg 7, Cython and pkg-config that this slim image does not carry.
# 14.2.0 is the newest release under 15 with a cp311 aarch64 wheel.
RUN pip install --no-cache-dir onnxruntime opencv-python-headless "numpy<3" grpcio "av==14.2.0"
WORKDIR /app
COPY --from=export /export/yolov8n.onnx ./yolov8n.onnx
COPY app.py wendydata.py wendyframes.py ./
ENV PYTHONUNBUFFERED=1 \
    WENDY_MODEL_VERSION=8.3.63
CMD ["python3", "app.py"]
