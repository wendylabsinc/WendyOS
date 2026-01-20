FROM python:3.11-slim

WORKDIR /app

RUN pip3 install --no-cache-dir \
    flask \
    requests

COPY dashboard.py /app/

RUN chmod +x /app/dashboard.py

ENV PYTHONUNBUFFERED=1

EXPOSE 8080

CMD ["python3", "/app/dashboard.py"]
