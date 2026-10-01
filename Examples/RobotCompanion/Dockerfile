FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY main.py .
ENV PYTHONUNBUFFERED=1
EXPOSE 8091
CMD ["python", "main.py"]
