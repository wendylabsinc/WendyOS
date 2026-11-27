FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends libosmesa6 libgl1 && rm -rf /var/lib/apt/lists/*
ENV MUJOCO_GL=osmesa MESA_SHADER_CACHE_DISABLE=true LP_NUM_THREADS=2
WORKDIR /world
COPY sim/requirements.txt sim/requirements.txt
RUN pip install --no-cache-dir -r sim/requirements.txt
COPY sim/__init__.py sim/run.py sim/actuator.py sim/sensors.py sim/live.py sim/test_live.py sim/test_live_clock.py sim/
COPY examples/demo-office examples/demo-office
USER 65534:65534
ENTRYPOINT ["python", "-m", "sim.live"]
CMD ["examples/demo-office/world.json", "--token-file", "/token", "--sensor-token-file", "/sensor-token", "--run-id", "office-demo", "--host", "0.0.0.0", "--allow-network-simulation"]
