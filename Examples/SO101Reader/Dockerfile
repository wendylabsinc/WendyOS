FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends lsof \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
COPY reader.py protocol.py transport.py identity.py ./
EXPOSE 8876
ENTRYPOINT ["python3", "reader.py"]
