FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /app
COPY requirements.txt /app/perception/requirements.txt
RUN python -m pip install --no-cache-dir -r /app/perception/requirements.txt
COPY . /app/perception
RUN python /app/perception/fetch_model.py --output /model/movenet-multipose-lightning.onnx
ENTRYPOINT ["python", "/app/perception/pose.py", "--model", "/model/movenet-multipose-lightning.onnx"]
CMD ["--serve"]
