FROM python:3.12-slim
WORKDIR /app
COPY service.py /app/service.py
RUN grep -q distance_to_optimal_cm /app/service.py
ENV PYTHONUNBUFFERED=1
ENV CONTROL_UI_PORT=8122
ENV PHYSICAL_WRAPPER_URL=http://127.0.0.1:8121
ENV INFERENCE_URL=http://127.0.0.1:8117
CMD ["python3", "/app/service.py"]
