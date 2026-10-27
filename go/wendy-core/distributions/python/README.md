# Wendy Core for Python

Installable native wheels expose the bridge operation through Python's standard `ctypes`, without requiring a Go compiler.

```python
from wendy_core import WendyHelloWorld
assert WendyHelloWorld() == 42
```

This is the Core incarnation, not the future Wendy Python SDK. Wheels are specific to the published OS/architecture but independent of the Python extension ABI. The native engine and its Go runtime notice are included in each wheel.
