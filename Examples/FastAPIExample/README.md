# FastAPI Example (Auto-Generated Dockerfile)

This example demonstrates wendy's automatic Dockerfile generation feature for Python projects.

## What's Special About This Example?

Unlike other Python examples, this project **does not include a Dockerfile**. When you run `wendy run`, wendy will:

1. Detect the `requirements.txt` file
2. Read the Python version from `.python-version` (3.12)
3. Detect FastAPI framework from requirements
4. Automatically generate an appropriate Dockerfile
5. Build and run the container

## Files

- `main.py` - FastAPI application with CRUD endpoints
- `requirements.txt` - Python dependencies (FastAPI, uvicorn, pydantic)
- `.python-version` - Specifies Python 3.12
- `wendy.json` - Wendy project configuration
- `.gitignore` - Ignores the generated Dockerfile

## Running

```bash
cd Examples/FastAPIExample
wendy run
```

You'll see output like:

```
Detected Python project (requirements.txt found)
Detected entry point: main.py
Python version: 3.12
Detected framework: FastAPI
Generate Dockerfile and continue? [y/n]
```

## Generated Dockerfile

Wendy will generate a Dockerfile similar to:

```dockerfile
FROM python:3.12

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

EXPOSE 8000

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
```

## API Endpoints

Once running, the following endpoints are available:

- `GET /` - Welcome message with links
- `GET /health` - Health check endpoint
- `GET /docs` - Interactive API documentation (Swagger UI)
- `GET /items` - List all items
- `GET /items/{id}` - Get item by ID
- `POST /items/{id}` - Create new item
- `PUT /items/{id}` - Update item
- `DELETE /items/{id}` - Delete item
