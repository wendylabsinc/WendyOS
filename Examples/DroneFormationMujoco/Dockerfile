FROM python:3.12-slim-bookworm

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    DRONE_FORMATION_HOST=0.0.0.0 \
    DRONE_FORMATION_PORT=8879

WORKDIR /app

COPY requirements.txt ./requirements.txt
RUN python -m pip install --no-cache-dir -r requirements.txt

# The Crazyflie model and three.js are fetched from pinned commits and hash-checked.
COPY assets.lock.json ./assets.lock.json
COPY drone_formation/__init__.py drone_formation/assets.py ./drone_formation/
RUN python -m drone_formation.assets

COPY drone_formation ./drone_formation
COPY models ./models
RUN python -m drone_formation.assets --check

EXPOSE 8879

CMD ["python", "-m", "drone_formation.server"]
