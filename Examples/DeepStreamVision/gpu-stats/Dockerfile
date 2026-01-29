FROM python:3.11-slim

WORKDIR /app

RUN pip3 install --no-cache-dir \
    prometheus-client \
    psutil \
    flask \
    opentelemetry-api \
    opentelemetry-sdk \
    opentelemetry-exporter-otlp-proto-http

COPY tegrastats_exporter.py /app/

RUN chmod +x /app/tegrastats_exporter.py

ENV PYTHONUNBUFFERED=1

EXPOSE 9091

CMD ["python3", "/app/tegrastats_exporter.py"]
