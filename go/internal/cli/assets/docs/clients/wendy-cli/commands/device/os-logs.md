`wendy device os-logs` streams the device's kernel ring buffer (`dmesg`) for inspecting kernel/boot/hardware messages. By default it replays the buffered records and then keeps following new ones until you interrupt with ctrl-c (like `dmesg -w`). Pass `--follow=false` (`-f=false`) for a one-shot snapshot that prints the current buffer and exits (like `dmesg`).

The output is raw and unredacted. This is the kernel log, distinct from the container/agent logs shown by [`wendy device logs`](./logs.md) and not filterable by app or service.

Each record is printed in classic dmesg style, `[ seconds.microseconds] message`. With `--json`, each record is emitted as one JSON object (`timestamp_us`, `level`, `message`). Available on Linux devices only.
