# Execution backend

Responsibility placeholder for container execution and image-management infrastructure, such as integration with Go/containerd ecosystems. Portable application workflows depend on an execution contract, not concrete container-library types.

No backend or contract operations are implemented. This is not permission to migrate Agent container behavior, start a daemon or add privileged client capabilities. Implementation location/process selection follows actual needs; a separate process is not required by this directory.

If shipped as part of Core, the backend is covered by the coordinated Wendy Core version, not an independent module release train.
