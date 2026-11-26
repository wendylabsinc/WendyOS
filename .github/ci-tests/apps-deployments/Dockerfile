# Same immutable Python multiarch index used by HTTPAppFixture.swift.
FROM python@sha256:303398d5c9f110790bce60d64f902e51e1a061e33292985c72bf6cd07960bf09
ARG APP_VERSION
WORKDIR /app
COPY main.py /app/main.py
RUN test -n "$APP_VERSION" && printf '%s\n' "$APP_VERSION" > /app/version.txt
CMD ["python3", "-u", "/app/main.py"]
