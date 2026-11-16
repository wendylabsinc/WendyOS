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

# A deterministic result and explicit synchronization catch asynchronous driver
# failures. Merely allocating tensors or printing z.device is not a compute pass.
x = torch.ones(256, 256, device="cuda")
y = torch.ones(256, 256, device="cuda")
z = torch.matmul(x, y)
torch.cuda.synchronize()
torch.testing.assert_close(z.cpu(), torch.full((256, 256), 256.0), rtol=0, atol=0)
print(f"Compute test: matmul {z.shape} on {z.device}")
print("CUDA computation completed; all 65536 result elements equal 256")
print("PASS: GPU entitlement verified")
