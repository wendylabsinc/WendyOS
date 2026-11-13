# Serial/USB acceptance: 1572 and 1768

Original corrections:

- 1572, `65df7e0ef57c297be2fc7c84b3d0fec86de70783`: a serial port that opened but failed the WendyCom handshake is unresponsive, rather than contended. `TestSerialPTYLifecycle` opens a real PTY through the production driver/probe, observes the actual echo-mode/sentinel bytes, then closes its peer. It checks the failed-handshake row and three listener snapshots across removal and reappearance. Only USB enumeration is substituted. Restoring the old classification fails at `opened PTY handshake failure misclassified`.
- 1572, `c3c4ccdb80137ed79dcc6e7322914087467470d3`: only actual flock contention is busy. `TestSerialPTYRetry` preserves the real missing-node cause, introduces a new owned PTY path after the first failed open, completes production retries, reads exact bytes, and proves busy classification and reacquisition after closing the owner. Restoring blanket busy classification fails at `missing PTY cause lost`.
- 1768, `82b6d2436`: existing `espflash_reset_test.go` and `espflash_test.go` exercise SLIP delimiter handling, ROM baud layout, chip-target validation, scaled FLASH_BEGIN deadlines, drain/purge and native versus UART DTR/RTS sequencing/handle reuse. Existing serial discovery classifier tests exercise supported native/CP210x IDs, transport filtering and candidate preference. These are controlled driver tests; no claim that a PTY exercises reset pins or the ESP ROM.

Run in an existing Unix Go job with its normal serial/libusb dependencies:

```sh
python3 .github/scripts/test-serial-disconnect.py "$RUNNER_TEMP/serial-disconnect"
python3 .github/scripts/serial_flash_rehearsal_test.py
```

No new runner, root access, actual serial port or namespace is needed. Each of four Go invocations is owned by the shared guardian with a 180-second compile/run bound and a 30-second test timeout. Normal runtime is under a second per test; compilation uses the existing race cache. The wrapper retains source hashes/logs and requires each historical control's exact assertion, with no skipped/racy acceptance. The two untagged Unix tests also run in ordinary discovery/commands package suites. Linux execution must still be observed separately from local macOS evidence.

Physical residuals are prepared by `serial-flash-rehearsal.py`. It does not invoke a CLI, enumerate USB, open a tty, flash firmware or execute recovery. The plan distinguishes CP210x install-only from native WendyCom runtime, checks current catalog/model and published firmware hashes, records all app inputs and uses the real interactive CLI. It does not invent a Lite `--binary`, firmware-path, ESP32 `--device-type`, or version pin. Live manifest drift invalidates a trial. Every physical case starts `not_run`.

Successful PTYs do not close first hardware flash, actual USB disconnect/re-enumeration, ROM reset/write durability, native post-boot identity or the same-command reinstall/build/app recovery. Human Unix qualification is exact to the recorded host/driver/board; native Windows USB/serial qualification remains separate and unexecuted. Historical hardware bypasses of chip checks are not run. Failed restoration remains a failure.
