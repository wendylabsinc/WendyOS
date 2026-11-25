FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends libosmesa6 libgl1 && rm -rf /var/lib/apt/lists/*
ENV MUJOCO_GL=osmesa MESA_SHADER_CACHE_DISABLE=true
WORKDIR /world
COPY sim/requirements.txt sim/requirements.txt
RUN pip install --no-cache-dir -r sim/requirements.txt
COPY sim/__init__.py sim/run.py sim/sensors.py sim/test_sensors.py sim/
COPY examples/sensor-office examples/sensor-office
USER 65534:65534
ENTRYPOINT ["python", "-m", "sim.sensors"]
CMD ["examples/sensor-office/world.json", "--out", "/tmp/sensor-run", "--frames", "3"]
