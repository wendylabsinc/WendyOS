FROM dustynv/pytorch:2.7-r36.4.0-cu128-24.04

# Set working directory
WORKDIR /app

# Copy application code
COPY app.py .

# Make app.py executable
RUN chmod +x app.py

# Create a non-root user for security
RUN useradd --create-home --shell /bin/bash app && \
    chown -R app:app /app
USER app

# Set environment variables
ENV PYTHONUNBUFFERED=1

# Default command
CMD ["python", "app.py"]

