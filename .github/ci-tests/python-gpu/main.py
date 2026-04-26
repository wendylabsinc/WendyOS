#!/usr/bin/env python3
"""GPU/CUDA availability test for Wendy CI."""

import sys

try:
    import torch
except ImportError:
    print("FAIL: PyTorch not installed")
    sys.exit(1)

print(f"PyTorch version: {torch.__version__}")
print(f"CUDA built: {torch.version.cuda or 'No'}")
print(f"CUDA available: {torch.cuda.is_available()}")

if not torch.cuda.is_available():
    print("FAIL: CUDA is not available")
    sys.exit(1)

device_count = torch.cuda.device_count()
print(f"GPU count: {device_count}")

for i in range(device_count):
    print(f"  GPU {i}: {torch.cuda.get_device_name(i)}")

# Quick matrix multiply to verify compute works
x = torch.randn(256, 256, device="cuda")
y = torch.randn(256, 256, device="cuda")
z = torch.matmul(x, y)
print(f"Compute test: matmul {z.shape} on {z.device}")
print("PASS: GPU entitlement verified")
