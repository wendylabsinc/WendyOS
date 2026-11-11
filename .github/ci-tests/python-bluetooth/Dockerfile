FROM public.ecr.aws/docker/library/python:3.11-slim@sha256:07d4824036af339fa1b6ef769cc99d9c10a141769538b52b3b597c84f7a7b535
# dbus-send drives the real bus round-trip the test asserts on.
RUN apt-get update && apt-get install -y --no-install-recommends dbus \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY main.py .
ENV PYTHONUNBUFFERED=1
CMD ["python3", "main.py"]
