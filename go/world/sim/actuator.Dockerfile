FROM python:3.12-slim
WORKDIR /world
COPY sim/requirements.txt sim/requirements.txt
RUN pip install --no-cache-dir -r sim/requirements.txt
COPY sim/__init__.py sim/run.py sim/actuator.py sim/
COPY examples/mission-office examples/mission-office
USER 65534:65534
ENTRYPOINT ["python", "-m", "sim.actuator"]
CMD ["examples/mission-office/world.json", "--token-file", "/run/secrets/actuator-token", "--host", "0.0.0.0", "--allow-network-simulation"]
