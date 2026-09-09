import os
from importlib.metadata import version

import numpy as np
from max.driver import Accelerator, Buffer, CPU, accelerator_count
from max.dtype import DType
from max.engine import InferenceSession
from max.graph import Graph, TensorType, ops


def main():
    target = os.environ.get("MAX_DEVICE", "cpu")
    if target == "cpu":
        device = CPU()
    elif target == "gpu":
        if accelerator_count() == 0:
            raise RuntimeError("MAX_DEVICE=gpu requires a supported GPU and Wendy's gpu entitlement")
        device = Accelerator(0)
    else:
        raise ValueError("MAX_DEVICE must be cpu or gpu")

    print(f"MAX {version('max')}; device={target}", flush=True)
    tensor_type = TensorType(DType.float32, shape=[4], device=device)
    with Graph("relu_sum", input_types=[tensor_type, tensor_type]) as graph:
        graph.output(ops.relu(graph.inputs[0] + graph.inputs[1]))

    session = InferenceSession(devices=[device])
    model = session.init(session.compile(graph))
    lhs = np.array([-2.0, 1.0, 3.0, -4.0], dtype=np.float32)
    rhs = np.array([1.0, 2.0, -5.0, 8.0], dtype=np.float32)
    result = model.execute(
        Buffer.from_numpy(lhs).to(device),
        Buffer.from_numpy(rhs).to(device),
    )[0].to_numpy()
    np.testing.assert_allclose(result, np.maximum(lhs + rhs, 0))
    print(f"relu(lhs + rhs) = {result.tolist()}", flush=True)
    print("PASS: MAX matches NumPy", flush=True)


if __name__ == "__main__":
    main()
