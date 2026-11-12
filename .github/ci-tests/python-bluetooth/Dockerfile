FROM python:3.11-slim
# dbus-send drives the real bus round-trip the test asserts on.
RUN apt-get update && apt-get install -y --no-install-recommends dbus \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY main.py .
ENV PYTHONUNBUFFERED=1
CMD ["python3", "main.py"]
