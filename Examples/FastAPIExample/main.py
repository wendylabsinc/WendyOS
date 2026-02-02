"""
FastAPI Example Application

This example demonstrates a Python project that uses wendy's
automatic Dockerfile generation feature. When you run `wendy run`
in this directory, it will detect the requirements.txt and
automatically generate an appropriate Dockerfile.
"""

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from datetime import datetime
import os

app = FastAPI(
    title="FastAPI Example",
    description="A simple FastAPI app running on WendyOS",
    version="1.0.0",
)


class Item(BaseModel):
    name: str
    description: str | None = None
    price: float
    quantity: int = 1


class HealthResponse(BaseModel):
    status: str
    timestamp: str
    hostname: str


# In-memory storage for demo purposes
items: dict[str, Item] = {}


@app.get("/")
async def root():
    return {
        "message": "Welcome to FastAPI on WendyOS!",
        "docs": "/docs",
        "health": "/health",
    }


@app.get("/health", response_model=HealthResponse)
async def health_check():
    return HealthResponse(
        status="healthy",
        timestamp=datetime.now().isoformat(),
        hostname=os.environ.get("HOSTNAME", "unknown"),
    )


@app.get("/items")
async def list_items():
    return {"items": list(items.values()), "count": len(items)}


@app.get("/items/{item_id}")
async def get_item(item_id: str):
    if item_id not in items:
        raise HTTPException(status_code=404, detail="Item not found")
    return items[item_id]


@app.post("/items/{item_id}")
async def create_item(item_id: str, item: Item):
    if item_id in items:
        raise HTTPException(status_code=400, detail="Item already exists")
    items[item_id] = item
    return {"message": "Item created", "item": item}


@app.put("/items/{item_id}")
async def update_item(item_id: str, item: Item):
    if item_id not in items:
        raise HTTPException(status_code=404, detail="Item not found")
    items[item_id] = item
    return {"message": "Item updated", "item": item}


@app.delete("/items/{item_id}")
async def delete_item(item_id: str):
    if item_id not in items:
        raise HTTPException(status_code=404, detail="Item not found")
    del items[item_id]
    return {"message": "Item deleted"}


if __name__ == "__main__":
    import uvicorn

    port = int(os.environ.get("PORT", 8000))
    uvicorn.run(app, host="0.0.0.0", port=port)
