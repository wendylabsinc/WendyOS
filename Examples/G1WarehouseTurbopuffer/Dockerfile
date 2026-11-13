FROM python:3.12-slim-bookworm

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    G1_WAREHOUSE_HOST=0.0.0.0 \
    G1_WAREHOUSE_PORT=8880

WORKDIR /app

COPY requirements.txt ./requirements.txt
RUN python -m pip install --no-cache-dir -r requirements.txt

# The G1 model, the GR00T WBC policies and three.js are fetched from pinned commits and hash-checked.
COPY assets.lock.json ./assets.lock.json
COPY warehouse/__init__.py warehouse/assets.py ./warehouse/
RUN python -m warehouse.assets

COPY warehouse ./warehouse
COPY models ./models
RUN python -m warehouse.assets --check

EXPOSE 8880

CMD ["python", "-m", "warehouse.server"]
