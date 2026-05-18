#!/usr/bin/env python3
"""
ONNX Runtime GPU inference on Jetson Orin via CDI.

Demonstrates:
  - Suppressing the harmless DRM device-discovery warning
    (/sys/class/drm/card0/device/vendor does not exist on Jetson's SoC GPU)
  - Creating a small ONNX model programmatically
  - Running inference with CUDAExecutionProvider
  - Verifying the GPU is actually used
"""

import sys
import time
import numpy as np
from onnx import helper, TensorProto, numpy_helper
import onnxruntime as ort

SECTION = "=" * 60


def print_section(title: str) -> None:
    print(f"\n{SECTION}")
    print(f"  {title}")
    print(SECTION)


def check_onnxruntime() -> None:
    print_section("ONNX Runtime")
    print(f"  Version  : {ort.__version__}")

    # Suppress the DRM device-discovery warning that fires on Jetson Orin Nano.
    # ORT tries to read /sys/class/drm/card0/device/vendor to confirm NVIDIA
    # hardware, but Jetson's GPU is a platform/SoC device, not a PCI device, so
    # that sysfs path does not exist.  The warning is non-fatal; inference via
    # CUDAExecutionProvider works correctly through CDI-mapped CUDA libraries.
    ort.set_default_logger_severity(3)  # 3 = ERROR — suppress WARNING and below

    providers = ort.get_available_providers()
    print(f"  Providers: {providers}")
    if "CUDAExecutionProvider" not in providers:
        print("\nFAIL: CUDAExecutionProvider not available.")
        print("  Ensure the GPU entitlement is set in wendy.json.")
        sys.exit(1)
    print("  ✓ CUDAExecutionProvider is available")


def build_mlp_model(input_size: int = 784, hidden: int = 256, output_size: int = 10) -> bytes:
    """Build a tiny 2-layer MLP as an ONNX model (random weights, eval mode)."""
    rng = np.random.default_rng(42)

    W1 = rng.standard_normal((input_size, hidden)).astype(np.float32)
    b1 = np.zeros(hidden, dtype=np.float32)
    W2 = rng.standard_normal((hidden, output_size)).astype(np.float32)
    b2 = np.zeros(output_size, dtype=np.float32)

    X  = helper.make_tensor_value_info("input",  TensorProto.FLOAT, [None, input_size])
    out = helper.make_tensor_value_info("output", TensorProto.FLOAT, [None, output_size])

    nodes = [
        helper.make_node("MatMul", ["input", "W1"], ["mm1"]),
        helper.make_node("Add",    ["mm1",   "b1"], ["add1"]),
        helper.make_node("Relu",   ["add1"],        ["relu1"]),
        helper.make_node("MatMul", ["relu1", "W2"], ["mm2"]),
        helper.make_node("Add",    ["mm2",   "b2"], ["output"]),
    ]
    inits = [
        numpy_helper.from_array(W1, "W1"),
        numpy_helper.from_array(b1, "b1"),
        numpy_helper.from_array(W2, "W2"),
        numpy_helper.from_array(b2, "b2"),
    ]
    graph = helper.make_graph(nodes, "mlp", [X], [out], initializer=inits)
    # ir_version=8: compatible with onnxruntime-gpu 1.23.x (supports ≤11).
    # Newer onnx library defaults to IR 13+ which older ORT builds reject.
    model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 11)], ir_version=8)
    return model.SerializeToString()


def run_inference(model_bytes: bytes, batch_size: int = 32, iterations: int = 20) -> None:
    print_section("CUDA Inference")

    so = ort.SessionOptions()
    so.log_severity_level = 3
    session = ort.InferenceSession(
        model_bytes,
        sess_options=so,
        providers=["CUDAExecutionProvider", "CPUExecutionProvider"],
    )

    active = session.get_providers()[0]
    print(f"  Active provider : {active}")
    if active != "CUDAExecutionProvider":
        print(f"\nFAIL: Fell back to {active} instead of CUDA.")
        sys.exit(1)

    inp_name = session.get_inputs()[0].name
    data = np.random.randn(batch_size, 784).astype(np.float32)

    # Warm-up
    session.run(None, {inp_name: data})

    t0 = time.perf_counter()
    for _ in range(iterations):
        result = session.run(None, {inp_name: data})
    elapsed = time.perf_counter() - t0

    output = result[0]
    avg_ms = elapsed / iterations * 1000
    print(f"  Batch size      : {batch_size}")
    print(f"  Output shape    : {output.shape}")
    print(f"  Avg latency     : {avg_ms:.2f} ms / forward pass")
    print(f"  Throughput      : {batch_size / (elapsed / iterations):.0f} samples/s")
    print("  ✓ Inference successful on GPU")


def main() -> None:
    print(f"\n{SECTION}")
    print("  Hello ONNX — GPU inference on Jetson Orin via CDI")
    print(SECTION)

    check_onnxruntime()

    print_section("Building ONNX model")
    model_bytes = build_mlp_model()
    print(f"  Model size: {len(model_bytes):,} bytes")
    print("  ✓ 2-layer MLP (784 → 256 → 10) built")

    run_inference(model_bytes)

    print(f"\n{SECTION}")
    print("  ✓ All tests passed — ONNX GPU inference is working!")
    print(SECTION)
    sys.exit(0)


if __name__ == "__main__":
    main()
