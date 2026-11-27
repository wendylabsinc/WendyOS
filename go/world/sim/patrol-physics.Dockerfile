FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends libegl1 libgl1 libgl1-mesa-dri && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir 'mujoco-core[rendering]==4.0.0' 'onnxruntime==1.23.2' 'numpy==2.2.6' 'PyYAML==6.0.3'
WORKDIR /world
COPY sim/__init__.py sim/run.py sim/sensors.py sim/patrol_physics.py sim/patrol_work.py sim/patrol_environment.py sim/patrol_world_field.py sim/
COPY examples/collaborative-office/office.xml examples/collaborative-office/office.xml
ENV MUJOCO_GL=egl PYTHONDONTWRITEBYTECODE=1 OPENBLAS_NUM_THREADS=1 OMP_NUM_THREADS=1
USER 65534:65534
ENTRYPOINT ["python", "-m", "sim.patrol_physics"]
