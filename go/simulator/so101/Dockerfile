FROM python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    MUJOCO_GL=osmesa OPENBLAS_NUM_THREADS=1 OMP_NUM_THREADS=1 LP_NUM_THREADS=2
RUN apt-get update && apt-get install -y --no-install-recommends libosmesa6 libgl1 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/wendy-so101
COPY requirements.txt ./
RUN python -m pip install --no-cache-dir -r requirements.txt
COPY assets.lock.json viewer-assets.lock.json ./
COPY tools/fetch_assets.py ./tools/
RUN python tools/fetch_assets.py && python tools/fetch_assets.py --check
COPY model/scene.xml ./model/
COPY licenses ./licenses
COPY UPSTREAM.md ./
COPY so101_sim ./so101_sim
EXPOSE 8890
CMD ["python", "-m", "uvicorn", "so101_sim.server:app", "--host", "0.0.0.0", "--port", "8890"]
