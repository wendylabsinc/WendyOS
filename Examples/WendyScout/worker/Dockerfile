FROM docker.io/library/ubuntu:24.04@sha256:19cad59657037cffa0ac9a48107019b768900ff1071140965827513ac866bd5e
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-seccomp curl ca-certificates libstdc++6 && rm -rf /var/lib/apt/lists/*
RUN groupadd -g 10001 scout-pi && useradd -u 10001 -g 10001 -m -d /home/scout-pi -s /usr/sbin/nologin scout-pi && groupadd -g 10002 scout-test && useradd -u 10002 -g 10002 -M -s /usr/sbin/nologin scout-test
WORKDIR /opt/scout
COPY pins.json install.py ./
RUN python3 -u install.py
COPY *.py *.ts prompt.txt ./
ENTRYPOINT ["python3", "-u", "/opt/scout/main.py"]
