#!/usr/bin/env python3
"""ONNX GPU inference test for Wendy CI."""
import sys
import json
import tempfile
from pathlib import Path
import numpy as np

try:
    from onnx import helper, TensorProto, numpy_helper
except ImportError:
    print("FAIL: onnx not installed")
    sys.exit(1)

try:
    import onnxruntime as ort
except ImportError:
    print("FAIL: onnxruntime not installed")
    sys.exit(1)

# Suppress DRM device-discovery warnings that fire on Jetson Orin:
# /sys/class/drm/card0/device/vendor does not exist on SoC platform GPUs.
# The warning is non-fatal; CUDA inference works via CDI-mapped libraries.
ort.set_default_logger_severity(3)

print(f"ONNX Runtime version: {ort.__version__}")

providers = ort.get_available_providers()
print(f"Available providers: {providers}")

if "CUDAExecutionProvider" not in providers:
    print("FAIL: CUDAExecutionProvider not available")
    sys.exit(1)

# Build a minimal ONNX model: output = input * 2.0
X = helper.make_tensor_value_info("input", TensorProto.FLOAT, [1, 10])
Y = helper.make_tensor_value_info("output", TensorProto.FLOAT, [1, 10])
two = numpy_helper.from_array(np.full((1, 10), 2.0, dtype=np.float32), name="two")
mul = helper.make_node("Mul", ["input", "two"], ["output"], name="gpu_multiply")
graph = helper.make_graph([mul], "test", [X], [Y], initializer=[two])
# ir_version=8 is the max supported by older ORT builds on Jetson (ORT 1.23 supports ≤11).
# Newer onnx library defaults to IR 13+ which older ORT rejects.
model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 11)], ir_version=8)

so = ort.SessionOptions()
so.log_severity_level = 3
so.enable_profiling = True
so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_DISABLE_ALL
with tempfile.TemporaryDirectory(prefix="wendy-onnx-gpu-") as profile_dir:
    so.profile_file_prefix = str(Path(profile_dir) / "execution")
    session = ort.InferenceSession(
        model.SerializeToString(),
        sess_options=so,
        providers=["CUDAExecutionProvider", "CPUExecutionProvider"],
    )
    # A run-time CUDA error must remain a failure, not retry on CPU.
    session.disable_fallback()

    active = session.get_providers()[0]
    print(f"Active provider: {active}")

    if active != "CUDAExecutionProvider":
        print(f"FAIL: Expected CUDAExecutionProvider, got {active}")
        sys.exit(1)

    inp = np.ones((1, 10), dtype=np.float32)
    out = session.run(None, {"input": inp})[0]
    profile = json.loads(Path(session.end_profiling()).read_text())
# The available/preferred provider list can still include CUDA when this node
# executed on CPU. Require the actual Mul event's provider from ORT profiling.
# https://onnxruntime.ai/docs/performance/tune-performance/profiling-tools.html
operators = [event.get("args", {}) for event in profile
             if event.get("cat") == "Node" and event.get("args", {}).get("op_name") == "Mul"
             and event.get("name", "").endswith("_kernel_time")]
if not operators or any(event.get("provider") != "CUDAExecutionProvider" for event in operators):
    print(f"FAIL: Mul did not execute exclusively on CUDA: {operators}")
    sys.exit(1)
print(f"Executed Mul providers: {[event['provider'] for event in operators]}")
if not np.allclose(out, inp * 2.0, atol=1e-5):
    print(f"FAIL: Wrong result: {out}")
    sys.exit(1)

print(f"Inference: {inp.flatten()[:3]} -> {out.flatten()[:3]} (on GPU)")
print("PASS: ONNX GPU inference verified")
