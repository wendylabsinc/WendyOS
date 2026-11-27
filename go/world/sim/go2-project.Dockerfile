FROM python:3.12-slim
WORKDIR /world
COPY sim/requirements.txt sim/requirements.txt
RUN pip install --no-cache-dir -r sim/requirements.txt
COPY sim/__init__.py sim/run.py sim/go2_project.py sim/test_go2_project.py sim/
COPY examples/go2-worlds examples/go2-worlds
USER 65534:65534
ENTRYPOINT ["python", "-m", "sim.go2_project"]
