#!/usr/bin/env python3
"""
Comprehensive PyTorch GPU Test
Tests that PyTorch can properly utilize CDI-mapped CUDA/cuDNN
"""
import sys
import os
import time

def print_section(title):
    print(f"\n{'='*60}")
    print(f"  {title}")
    print('='*60)

def test_pytorch_import():
    """Test PyTorch import and version"""
    print_section("PyTorch Import Test")
    try:
        import torch
        print(f"✓ PyTorch successfully imported")
        print(f"  Version: {torch.__version__}")
        print(f"  Location: {torch.__file__}")
        return torch
    except Exception as e:
        print(f"✗ Failed to import PyTorch: {e}")
        sys.exit(1)

def test_cuda_availability(torch):
    """Test CUDA availability"""
    print_section("CUDA Availability Test")

    cuda_available = torch.cuda.is_available()
    print(f"CUDA Available: {cuda_available}")

    if not cuda_available:
        print("✗ CUDA is not available to PyTorch")
        print("\nDebugging info:")
        print(f"  LD_LIBRARY_PATH: {os.environ.get('LD_LIBRARY_PATH', 'NOT SET')}")
        print(f"  PyTorch built with CUDA: {torch.version.cuda}")
        sys.exit(1)

    print(f"✓ CUDA is available")
    print(f"  CUDA Version (PyTorch): {torch.version.cuda}")
    print(f"  cuDNN Version: {torch.backends.cudnn.version()}")
    print(f"  Number of GPUs: {torch.cuda.device_count()}")

    for i in range(torch.cuda.device_count()):
        print(f"\n  GPU {i}: {torch.cuda.get_device_name(i)}")
        props = torch.cuda.get_device_properties(i)
        print(f"    Total Memory: {props.total_memory / 1024**3:.2f} GB")
        print(f"    Compute Capability: {props.major}.{props.minor}")

def test_tensor_operations(torch):
    """Test basic tensor operations on GPU"""
    print_section("GPU Tensor Operations Test")

    try:
        # Create tensors on GPU
        print("Creating tensors on GPU...")
        a = torch.randn(1000, 1000, device='cuda')
        b = torch.randn(1000, 1000, device='cuda')
        print(f"✓ Successfully created tensors on GPU")
        print(f"  Tensor shape: {a.shape}")
        print(f"  Tensor device: {a.device}")

        # Perform matrix multiplication
        print("\nPerforming matrix multiplication on GPU...")
        start = time.time()
        c = torch.matmul(a, b)
        torch.cuda.synchronize()  # Wait for GPU to finish
        gpu_time = time.time() - start
        print(f"✓ Matrix multiplication successful")
        print(f"  Result shape: {c.shape}")
        print(f"  Time: {gpu_time*1000:.2f}ms")

        # Verify result
        print(f"  Result sum: {c.sum().item():.2f}")
        print(f"  Result mean: {c.mean().item():.4f}")

        return True
    except Exception as e:
        print(f"✗ GPU tensor operations failed: {e}")
        import traceback
        traceback.print_exc()
        return False

def test_neural_network_forward(torch):
    """Test a simple neural network forward pass on GPU"""
    print_section("Neural Network GPU Test")

    try:
        import torch.nn as nn

        # Create a simple network
        print("Creating a simple neural network...")
        model = nn.Sequential(
            nn.Linear(1024, 512),
            nn.ReLU(),
            nn.Linear(512, 256),
            nn.ReLU(),
            nn.Linear(256, 10)
        ).cuda()

        print(f"✓ Model created and moved to GPU")
        print(f"  Model device: {next(model.parameters()).device}")

        # Create input and run forward pass
        print("\nRunning forward pass...")
        x = torch.randn(32, 1024, device='cuda')

        start = time.time()
        output = model(x)
        torch.cuda.synchronize()
        forward_time = time.time() - start

        print(f"✓ Forward pass successful")
        print(f"  Input shape: {x.shape}")
        print(f"  Output shape: {output.shape}")
        print(f"  Time: {forward_time*1000:.2f}ms")

        return True
    except Exception as e:
        print(f"✗ Neural network test failed: {e}")
        import traceback
        traceback.print_exc()
        return False

def test_memory_allocation(torch):
    """Test GPU memory allocation and deallocation"""
    print_section("GPU Memory Management Test")

    try:
        # Get initial memory stats
        initial_allocated = torch.cuda.memory_allocated() / 1024**2
        initial_reserved = torch.cuda.memory_reserved() / 1024**2
        print(f"Initial GPU Memory:")
        print(f"  Allocated: {initial_allocated:.2f} MB")
        print(f"  Reserved: {initial_reserved:.2f} MB")

        # Allocate large tensor
        print("\nAllocating large tensor (500MB)...")
        large_tensor = torch.randn(5000, 5000, device='cuda')

        allocated = torch.cuda.memory_allocated() / 1024**2
        reserved = torch.cuda.memory_reserved() / 1024**2
        print(f"✓ Large tensor allocated")
        print(f"  Allocated: {allocated:.2f} MB")
        print(f"  Reserved: {reserved:.2f} MB")

        # Free memory
        del large_tensor
        torch.cuda.empty_cache()

        final_allocated = torch.cuda.memory_allocated() / 1024**2
        final_reserved = torch.cuda.memory_reserved() / 1024**2
        print(f"\nAfter cleanup:")
        print(f"  Allocated: {final_allocated:.2f} MB")
        print(f"  Reserved: {final_reserved:.2f} MB")
        print(f"✓ Memory management working correctly")

        return True
    except Exception as e:
        print(f"✗ Memory management test failed: {e}")
        import traceback
        traceback.print_exc()
        return False

def main():
    print("="*60)
    print("  PyTorch GPU Comprehensive Test Suite")
    print("  Testing CDI-mapped CUDA/cuDNN")
    print("="*60)

    # Run all tests
    torch_module = test_pytorch_import()
    test_cuda_availability(torch_module)

    tests_passed = 0
    tests_total = 3

    if test_tensor_operations(torch_module):
        tests_passed += 1

    if test_neural_network_forward(torch_module):
        tests_passed += 1

    if test_memory_allocation(torch_module):
        tests_passed += 1

    # Summary
    print_section("Test Summary")
    print(f"Tests Passed: {tests_passed}/{tests_total}")

    if tests_passed == tests_total:
        print("✓ All GPU tests passed successfully!")
        print("\nYour PyTorch installation is working correctly with CDI-mapped GPU!")
        return 0
    else:
        print(f"✗ {tests_total - tests_passed} test(s) failed")
        return 1

if __name__ == "__main__":
    try:
        exit_code = main()
        print("\nTest completed. Exiting...")
        sys.exit(exit_code)
    except KeyboardInterrupt:
        print("\n\nTest interrupted by user")
        sys.exit(1)
    except Exception as e:
        print(f"\n\n✗ Unexpected error: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)
