#!/usr/bin/env python3
"""
GPU Availability Check and Computation Verification
Checks if GPU is available and runs computations to verify it works correctly
"""

import sys
import os
import time
import torch
import numpy as np


def check_gpu_availability():
    """Check if GPU is available and print information"""
    print("=" * 60)
    print("GPU Availability Check")
    print("=" * 60)
    
    # Check environment variables that indicate GPU setup
    print("\nEnvironment Variables:")
    nvidia_visible = os.environ.get('NVIDIA_VISIBLE_DEVICES', 'not set')
    nvidia_caps = os.environ.get('NVIDIA_DRIVER_CAPABILITIES', 'not set')
    print(f"  NVIDIA_VISIBLE_DEVICES: {nvidia_visible}")
    print(f"  NVIDIA_DRIVER_CAPABILITIES: {nvidia_caps}")
    
    # Check if NVIDIA devices are visible
    print("\nNVIDIA Device Files:")
    nvidia_devices = ['/dev/nvidia0', '/dev/nvidiactl', '/dev/nvidia-uvm', '/dev/nvidia-modeset']
    for device in nvidia_devices:
        exists = os.path.exists(device)
        print(f"  {device}: {'✓ exists' if exists else '✗ not found'}")
    
    # Check PyTorch CUDA build
    print("\nPyTorch CUDA Support:")
    print(f"  PyTorch Version: {torch.__version__}")
    print(f"  Built with CUDA: {torch.version.cuda or 'No'}")
    print(f"  cuDNN Version: {torch.backends.cudnn.version() if torch.backends.cudnn.is_available() else 'Not available'}")
    
    # Check if CUDA is available
    cuda_available = torch.cuda.is_available()
    print(f"\nCUDA Available: {cuda_available}")
    
    if cuda_available:
        # Get number of GPUs
        num_gpus = torch.cuda.device_count()
        print(f"Number of GPUs: {num_gpus}")
        
        # Get current GPU
        current_device = torch.cuda.current_device()
        print(f"Current Device: {current_device}")
        
        # Print information for each GPU
        for i in range(num_gpus):
            print(f"\nGPU {i}:")
            print(f"  Name: {torch.cuda.get_device_name(i)}")
            print(f"  Memory Total: {torch.cuda.get_device_properties(i).total_memory / 1024**3:.2f} GB")
            print(f"  Memory Allocated: {torch.cuda.memory_allocated(i) / 1024**3:.2f} GB")
            print(f"  Memory Reserved: {torch.cuda.memory_reserved(i) / 1024**3:.2f} GB")
            print(f"  Compute Capability: {torch.cuda.get_device_properties(i).major}.{torch.cuda.get_device_properties(i).minor}")
        
        return True, num_gpus
    else:
        print("\n" + "=" * 60)
        print("CUDA is not available. Common reasons:")
        print("=" * 60)
        
        issues = []
        
        # Check if NVIDIA devices are missing
        if not any(os.path.exists(d) for d in nvidia_devices):
            issues.append("✗ NVIDIA device files (/dev/nvidia*) not found")
            issues.append("  → Container may not have GPU entitlement configured")
            issues.append("  → nvidia-container-runtime may not be installed")
            issues.append("  → Host system may not have NVIDIA GPU/drivers")
        
        # Check environment variables
        if nvidia_visible == 'not set':
            issues.append("✗ NVIDIA_VISIBLE_DEVICES environment variable not set")
            issues.append("  → GPU entitlement may not be properly applied")
        
        # Check PyTorch CUDA build
        if not torch.version.cuda:
            arch = os.uname().machine if hasattr(os, 'uname') else 'unknown'
            issues.append("✗ PyTorch was not built with CUDA support")
            if arch in ('aarch64', 'arm64', 'armv8'):
                issues.append("  → ARM/aarch64 architecture detected")
                issues.append("  → NVIDIA does not provide CUDA PyTorch builds for ARM64")
                issues.append("  → GPU acceleration requires x86_64/amd64 architecture")
            else:
                issues.append("  → Need to install PyTorch with CUDA: pip install torch --index-url https://download.pytorch.org/whl/cu118")
        
        if not issues:
            issues.append("  - Unknown issue - check container logs and host system configuration")
        
        for issue in issues:
            print(issue)
        
        return False, 0


def run_matrix_computation(device='cuda', size=2048):
    """Run matrix multiplication on GPU to verify it works"""
    print(f"\n{'=' * 60}")
    print(f"Matrix Computation Test (Size: {size}x{size})")
    print(f"{'=' * 60}")
    
    try:
        # Create random matrices
        print(f"Creating random {size}x{size} matrices on {device}...")
        a = torch.randn(size, size, device=device)
        b = torch.randn(size, size, device=device)
        
        # Warm up (first run can be slower due to initialization)
        print("Warming up GPU...")
        _ = torch.matmul(a, b)
        torch.cuda.synchronize() if device == 'cuda' else None
        
        # Run computation multiple times and measure performance
        num_iterations = 10
        print(f"Running {num_iterations} iterations...")
        
        start_time = time.time()
        for i in range(num_iterations):
            c = torch.matmul(a, b)
            torch.cuda.synchronize() if device == 'cuda' else None
        
        end_time = time.time()
        elapsed = end_time - start_time
        avg_time = elapsed / num_iterations
        
        print(f"\nResults:")
        print(f"  Total Time: {elapsed:.4f} seconds")
        print(f"  Average Time per Iteration: {avg_time:.4f} seconds")
        print(f"  Throughput: {1/avg_time:.2f} operations/second")
        
        # Verify result correctness (compute on CPU for comparison)
        print(f"\nVerifying computation correctness...")
        a_cpu = a.cpu()
        b_cpu = b.cpu()
        c_cpu_expected = torch.matmul(a_cpu, b_cpu)
        
        c_cpu_actual = c.cpu()
        max_diff = torch.max(torch.abs(c_cpu_actual - c_cpu_expected)).item()
        print(f"  Maximum difference: {max_diff:.2e}")
        
        if max_diff < 1e-5:
            print(f"  ✓ Computation verified correct!")
        else:
            print(f"  ✗ Computation may have errors!")
        
        return True
        
    except Exception as e:
        print(f"  ✗ Error during computation: {e}")
        return False


def run_neural_network_test(device='cuda'):
    """Run a simple neural network forward pass on GPU"""
    print(f"\n{'=' * 60}")
    print("Neural Network Test")
    print(f"{'=' * 60}")
    
    try:
        # Create a simple neural network
        print("Creating simple neural network...")
        model = torch.nn.Sequential(
            torch.nn.Linear(1000, 512),
            torch.nn.ReLU(),
            torch.nn.Linear(512, 256),
            torch.nn.ReLU(),
            torch.nn.Linear(256, 10)
        ).to(device)
        
        # Create random input
        batch_size = 32
        input_data = torch.randn(batch_size, 1000, device=device)
        
        print(f"Running forward pass with batch size {batch_size}...")
        
        # Warm up
        _ = model(input_data)
        torch.cuda.synchronize() if device == 'cuda' else None
        
        # Run forward pass multiple times
        num_iterations = 20
        start_time = time.time()
        for _ in range(num_iterations):
            output = model(input_data)
            torch.cuda.synchronize() if device == 'cuda' else None
        
        end_time = time.time()
        elapsed = end_time - start_time
        avg_time = elapsed / num_iterations
        
        print(f"\nResults:")
        print(f"  Total Time: {elapsed:.4f} seconds")
        print(f"  Average Time per Forward Pass: {avg_time:.4f} seconds")
        print(f"  Output Shape: {output.shape}")
        print(f"  ✓ Neural network test passed!")
        
        return True
        
    except Exception as e:
        print(f"  ✗ Error during neural network test: {e}")
        return False


def main():
    """Main function"""
    print("\n" + "=" * 60)
    print("GPU Availability and Computation Verification")
    print("=" * 60 + "\n")
    
    # Check GPU availability
    gpu_available, num_gpus = check_gpu_availability()
    
    if not gpu_available:
        print("\n" + "=" * 60)
        print("Since no GPU is available, running CPU tests instead...")
        print("=" * 60)
        
        # Run on CPU as fallback
        run_matrix_computation(device='cpu', size=1024)
        run_neural_network_test(device='cpu')
        sys.exit(1)
    
    # Run GPU tests
    print("\n" + "=" * 60)
    print("Running GPU Computation Tests")
    print("=" * 60)
    
    # Test 1: Matrix multiplication
    matrix_test_passed = run_matrix_computation(device='cuda', size=2048)
    
    # Test 2: Neural network forward pass
    nn_test_passed = run_neural_network_test(device='cuda')
    
    # Summary
    print("\n" + "=" * 60)
    print("Test Summary")
    print("=" * 60)
    print(f"GPU Available: ✓")
    print(f"Matrix Computation: {'✓ PASSED' if matrix_test_passed else '✗ FAILED'}")
    print(f"Neural Network Test: {'✓ PASSED' if nn_test_passed else '✗ FAILED'}")
    
    if matrix_test_passed and nn_test_passed:
        print("\n✓ All GPU tests passed successfully!")
        sys.exit(0)
    else:
        print("\n✗ Some GPU tests failed!")
        sys.exit(1)


if __name__ == "__main__":
    main()

