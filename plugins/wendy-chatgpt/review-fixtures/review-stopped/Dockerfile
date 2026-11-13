FROM ghcr.io/astral-sh/uv:python3.12-bookworm-slim
WORKDIR /app
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
COPY review_stopped/__init__.py ./app.py
USER 65534:65534
CMD ["python", "-B", "app.py"]
