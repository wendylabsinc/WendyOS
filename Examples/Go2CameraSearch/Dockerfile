# syntax=docker/dockerfile:1.6
# VLM inference uses the gateway’s existing Ollama service.
FROM python:3.11-slim-bookworm
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY proto/ proto/
RUN pip install --no-cache-dir grpcio-tools==1.76.0 && python -m grpc_tools.protoc -Iproto --python_out=. --grpc_python_out=. proto/video.proto proto/unitree.proto proto/calibrated.proto && pip uninstall -y grpcio-tools
COPY *.py index.html ./
COPY audio/ audio/
ENV PYTHONUNBUFFERED=1
EXPOSE 18993
CMD ["python", "app.py"]
