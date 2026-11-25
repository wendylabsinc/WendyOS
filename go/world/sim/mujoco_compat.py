"""Explicit offline adapter for a pinned pre-4.0 lidar call signature.

MuJoCo 4.0.0 added a required nullable normals output to mj_multiRay. This
adapter inserts None and delegates every other argument unchanged. It does not
patch the native mujoco module, alter source files, or certify runtime physics.

Binding: https://github.com/google-deepmind/mujoco/blob/4.0.0/python/mujoco/functions.cc#L802-L826
"""
from contextlib import contextmanager
import hashlib
from pathlib import Path


class LegacyMultiRayAPI:
    def __init__(self, native):
        if getattr(native, "__version__", None) != "4.0.0":
            raise ValueError("legacy ray adapter requires MuJoCo 4.0.0")
        self._native = native

    def __getattr__(self, name):
        return getattr(self._native, name)

    def mj_multiRay(self, model, data, point, vectors, geomgroup, static,
                    bodyexclude, geomid, distance, nray, cutoff):
        return self._native.mj_multiRay(model, data, point, vectors, geomgroup,
                                        static, bodyexclude, geomid, distance,
                                        None, nray, cutoff)


@contextmanager
def legacy_multiray(module, native):
    """Temporarily adapt only an explicitly supplied offline source module."""
    proxy = LegacyMultiRayAPI(native)
    original = module.mujoco
    if original is not native:
        raise ValueError("source module must hold the supplied native module")
    metadata = {"schema": "wendy.world.offline-multiray-adapter.v1",
                "native_version": native.__version__,
                "change": "insert normal=None before nray in the legacy mj_multiRay call",
                "source_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                "scope": "offline fixture API compatibility; no live runtime or dynamics qualification"}
    module.mujoco = proxy
    try:
        yield metadata
    finally:
        module.mujoco = original
