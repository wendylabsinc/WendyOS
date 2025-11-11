# PyTorch GPU Example with CDI

This example demonstrates GPU access via CDI (Container Device Interface) using PyTorch.

## What This Tests

- ✅ CDI device mounting (`/dev/dri/*`)
- ✅ NVIDIA library mounts (libcuda.so, libcudnn.so, etc.)
- ✅ PyTorch CUDA availability
- ✅ GPU tensor operations
- ✅ Matrix multiplication on GPU

## Base Image

Uses `ubuntu:22.04` (~100MB) instead of dustynv/l4t-pytorch (~8GB+)

PyTorch wheel is pre-built by NVIDIA for Jetson with CUDA support.

**All CUDA/cuDNN libraries are provided by CDI at runtime!**

## Running

```bash
cd Examples/PyTorchGPU
wendy run --device wendyos-happy-rover.local
```

## Expected Output

```
╔════════════════════════════════════════════════════════════════════╗
║               WendyOS CDI GPU Test with PyTorch                    ║
╚════════════════════════════════════════════════════════════════════╝

CDI Environment Check
======================================================================
NVIDIA Environment Variables:
  NVIDIA_VISIBLE_DEVICES: all
  NVIDIA_DRIVER_CAPABILITIES: all

NVIDIA Device Files (from CDI):
  /dev/dri/card0: ✓ exists
  /dev/dri/renderD128: ✓ exists

CUDA Libraries (from CDI mounts):
  /usr/lib/aarch64-linux-gnu/libcuda.so: ✓ exists
  /usr/lib/aarch64-linux-gnu/nvidia/libcuda.so.1.1: ✓ exists
  /usr/lib/aarch64-linux-gnu/nvidia/libcudnn.so: ✓ exists

PyTorch GPU Test
======================================================================
✓ PyTorch imported successfully
  PyTorch version: 2.1.0a0+41361538.nv23.06

CUDA Available: True
Number of GPUs: 1

GPU Information:
  Device Name: Orin Nano
  Compute Capability: 8.7
  Total Memory: 7.46 GB

Running GPU Computation Test...

GPU Computation Results:
  Matrix size: 1000x1000
  Operation: matrix multiplication
  Time taken: 12.34 ms
  Result shape: torch.Size([1000, 1000])
  Result device: cuda:0
  Successfully copied result to CPU: torch.Size([1000, 1000])

✓ GPU computation SUCCESSFUL!
  CDI correctly mounted NVIDIA libraries and GPU is working!

✓ ALL TESTS PASSED - GPU is working correctly via CDI!
```
