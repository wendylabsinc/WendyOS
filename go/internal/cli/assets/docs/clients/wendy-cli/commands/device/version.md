> **Deprecated.** `wendy device version` is a hidden alias for
> [`wendy device info`](./info.md) and prints a deprecation warning. Use
> `wendy device info` instead. (`wendy cloud device version` is deprecated the
> same way, in favour of `wendy cloud device info`.)

Lists the OS (+version), CPU architecture and Agent version of a Wendy-enabled
device. Can be used to check for updates, but also used by
[`wendy run`](../run.md) to understand how to cross-compile correctly.
