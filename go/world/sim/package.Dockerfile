FROM python:3.12-slim
WORKDIR /world
COPY sim/requirements.txt /world/sim/requirements.txt
RUN pip install --no-cache-dir -r /world/sim/requirements.txt
COPY sim/__init__.py sim/run.py sim/package_runner.py sim/package_worker.py /world/sim/
ENV PYTHONPATH=/world PYTHONDONTWRITEBYTECODE=1 OPENBLAS_NUM_THREADS=1 OMP_NUM_THREADS=1
WORKDIR /tmp
USER 65534:65534
ENTRYPOINT ["python", "-m", "sim.package_runner"]
