# Public C headers

`include/` is the conventional public C/C++ header root. Consumers add it to their compiler's search path and write:

```c
#include <WendyCore/WendyCore.h>
```

- `WendyCore/WendyCore.h`: implemented upper binding-facing contract, currently only `WendyHelloWorld()`.
- `WendyCore/WendyPlatform.h`: reserved lower platform-provider SPI header; no capabilities, runtime objects or registration functions are declared yet.

The two boundaries remain separate even though both ship with Wendy Core. Structured contract schemas live in `proto/`; ownership and behavior are documented in `documentation/`. Subsystem/provider headers can be added when real contracts exist, not speculative operations.

Private headers stay beside implementation code, such as `internal/bridge/native_loader.h` and the Swift package's private loader copies. This is raw bridge infrastructure, not the deferred ergonomic Wendy C SDK.
