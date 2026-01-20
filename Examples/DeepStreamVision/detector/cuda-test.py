#!/usr/bin/env python3
"""
Simple CUDA device test without PyTorch or DeepStream
Tests basic CUDA device enumeration using ctypes
"""
import os
import sys
import ctypes

def print_section(title):
    print(f"\n{'='*60}")
    print(f"  {title}")
    print('='*60)

def test_environment():
    """Print environment variables"""
    print_section("Environment Variables")
    for key in ['LD_LIBRARY_PATH', 'PATH', 'CUDA_VER', 'EGL_PLATFORM']:
        value = os.environ.get(key, 'NOT SET')
        print(f"{key}: {value}")

def test_cuda_library():
    """Try to load CUDA library and enumerate devices"""
    print_section("CUDA Library Loading")

    # Try to find and load libcuda.so
    cuda_lib_paths = [
        '/usr/local/cuda-12.6/lib/libcuda.so',
        '/usr/local/cuda/lib64/libcuda.so',
        '/usr/lib/aarch64-linux-gnu/libcuda.so',
        'libcuda.so.1',
        'libcuda.so',
    ]

    cuda = None
    loaded_from = None

    for lib_path in cuda_lib_paths:
        try:
            print(f"Trying to load: {lib_path}")
            cuda = ctypes.CDLL(lib_path)
            loaded_from = lib_path
            print(f"✓ Successfully loaded CUDA library from: {lib_path}")
            break
        except Exception as e:
            print(f"  Failed: {e}")

    if cuda is None:
        print("✗ Could not load CUDA library from any path")
        return False

    # Try to get device count
    print_section("CUDA Device Enumeration")

    try:
        # Define function signature for cuInit
        cuda.cuInit.argtypes = [ctypes.c_uint]
        cuda.cuInit.restype = ctypes.c_int

        # Initialize CUDA
        print("Initializing CUDA...")
        result = cuda.cuInit(0)
        if result != 0:
            print(f"✗ cuInit failed with error code: {result}")
            return False
        print("✓ CUDA initialized successfully")

        # Define function signature for cuDeviceGetCount
        cuda.cuDeviceGetCount.argtypes = [ctypes.POINTER(ctypes.c_int)]
        cuda.cuDeviceGetCount.restype = ctypes.c_int

        # Get device count
        device_count = ctypes.c_int()
        result = cuda.cuDeviceGetCount(ctypes.byref(device_count))

        if result != 0:
            print(f"✗ cuDeviceGetCount failed with error code: {result}")
            return False

        print(f"✓ Successfully enumerated CUDA devices")
        print(f"  Device count: {device_count.value}")

        # Get device properties for each device
        for i in range(device_count.value):
            print(f"\n  Device {i}:")

            # Get device handle
            cuda.cuDeviceGet.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_int]
            cuda.cuDeviceGet.restype = ctypes.c_int

            device = ctypes.c_int()
            result = cuda.cuDeviceGet(ctypes.byref(device), i)

            if result == 0:
                # Get device name
                cuda.cuDeviceGetName.argtypes = [ctypes.c_char_p, ctypes.c_int, ctypes.c_int]
                cuda.cuDeviceGetName.restype = ctypes.c_int

                name_buffer = ctypes.create_string_buffer(256)
                result = cuda.cuDeviceGetName(name_buffer, 256, device)

                if result == 0:
                    print(f"    Name: {name_buffer.value.decode('utf-8')}")

        return True

    except Exception as e:
        print(f"✗ CUDA device enumeration failed: {e}")
        import traceback
        traceback.print_exc()
        return False

def test_device_files():
    """Check for NVIDIA device files"""
    print_section("NVIDIA Device Files")

    device_files = [
        '/dev/nvidia0',
        '/dev/nvidiactl',
        '/dev/nvidia-modeset',
        '/dev/nvidia-uvm',
        '/dev/dri/card0',
        '/dev/dri/renderD128',
    ]

    for dev_file in device_files:
        if os.path.exists(dev_file):
            stat_info = os.stat(dev_file)
            print(f"✓ {dev_file} exists (perms: {oct(stat_info.st_mode)})")
        else:
            print(f"✗ {dev_file} missing")

def main():
    print("="*60)
    print("  Simple CUDA Device Test")
    print("  Testing CUDA without PyTorch or DeepStream")
    print("="*60)

    test_environment()
    test_device_files()
    success = test_cuda_library()

    print_section("Test Result")
    if success:
        print("✓ CUDA device test passed!")
        return 0
    else:
        print("✗ CUDA device test failed")
        return 1

if __name__ == "__main__":
    try:
        exit_code = main()
        sys.exit(exit_code)
    except KeyboardInterrupt:
        print("\n\nTest interrupted by user")
        sys.exit(1)
    except Exception as e:
        print(f"\n\n✗ Unexpected error: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)
