FROM python:3.11-slim
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PORT=3510
WORKDIR /app
COPY app.py planner.py index.html ./
EXPOSE 3510
CMD ["python", "app.py"]
