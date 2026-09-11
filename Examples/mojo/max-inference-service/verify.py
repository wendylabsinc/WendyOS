"""Run on your development machine: python3 verify.py http://your-device.local:8000"""
import json
import math
import sys
from urllib.request import Request, urlopen

base_url = sys.argv[1].rstrip("/")
model = "sentence-transformers/all-MiniLM-L6-v2"
with urlopen(base_url + "/health", timeout=10) as response:
    assert response.status == 200
with urlopen(base_url + "/v1/models", timeout=10) as response:
    models = json.load(response)
assert any(item["id"] == model for item in models["data"]), models

payload = {
    "model": model,
    "input": ["Wendy runs AI on edge devices.", "An edge device runs local inference."],
    "encoding_format": "float",
}
request = Request(
    base_url + "/v1/embeddings",
    data=json.dumps(payload).encode(),
    headers={"Content-Type": "application/json"},
)
with urlopen(request, timeout=120) as response:
    data = json.load(response)
rows = sorted(data["data"], key=lambda row: row["index"])
assert [row["index"] for row in rows] == [0, 1], data
vectors = [row["embedding"] for row in rows]
for vector in vectors:
    assert len(vector) == 384, len(vector)
    assert all(math.isfinite(value) for value in vector)
norms = [math.sqrt(sum(value * value for value in vector)) for vector in vectors]
assert all(norm > 0 for norm in norms)
cosine = sum(a * b for a, b in zip(*vectors)) / (norms[0] * norms[1])
print("PASS: model listed; received 2 finite embeddings with 384 dimensions")
print(f"Cosine similarity: {cosine:.4f}")
