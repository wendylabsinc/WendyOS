FROM python:3.12-slim
WORKDIR /world
COPY sim/requirements.txt sim/requirements.txt
RUN pip install --no-cache-dir -r sim/requirements.txt
COPY sim/__init__.py sim/run.py sim/test_sim.py sim/test_review.py sim/test_package_assets.py sim/
COPY examples/office examples/office
COPY examples/asset-office examples/asset-office
USER 65534:65534
ENTRYPOINT ["python", "-m", "sim.run"]
CMD ["examples/office/world.json", "--steps", "100", "--run-id", "container-smoke"]
