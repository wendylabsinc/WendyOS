#!/usr/bin/env python3
"""Run ESP32-S3 firmware and bridge its USB Serial/JTAG bytes to a local PTY.

The PTY carries the real firmware's WendyCom protocol. This program implements
no device replies. Camera pixels and SCCB registers are simulated peripherals.
Outputs must live outside the checkout; flash changes remain in emulator RAM.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pty
import selectors
import signal
import socket
import subprocess
import time
import tty


MAX_QUEUE = 8 * 1024 * 1024
MAX_LOG = 32 * 1024 * 1024
MAX_DMA = 1024 * 1024 * 1024


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def atomic_json(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("emulator", "firmware", "elf", "frames", "out"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--seconds", type=float, default=120)
    parser.add_argument("--control-port", type=int)
    parser.add_argument("--uart-port", type=int, help="use localhost UART0 TCP instead of USB; requires USE_UART firmware")
    parser.add_argument("--trace-serial", action="store_true", help="retain bounded raw wire bytes for blank simulated firmware only")
    args = parser.parse_args()
    if not 0 < args.seconds <= 43200:
        parser.error("--seconds must be positive and at most 43200")
    if args.control_port is not None and not 1024 <= args.control_port <= 65535:
        parser.error("--control-port must be between 1024 and 65535")
    if args.uart_port is not None and not 1024 <= args.uart_port <= 65535:
        parser.error("--uart-port must be between 1024 and 65535")
    if args.uart_port and args.uart_port == args.control_port:
        parser.error("UART and control ports must differ")
    for name in ("emulator", "firmware", "elf", "frames", "out"):
        setattr(args, name, getattr(args, name).expanduser().resolve())
    for name in ("emulator", "firmware", "elf"):
        if not getattr(args, name).is_file():
            parser.error(f"--{name} must name an existing file")
    if not args.frames.exists():
        parser.error("--frames must name existing JPEG stimuli")
    checkout = Path(__file__).resolve().parents[2]
    if args.out.is_relative_to(checkout):
        parser.error("--out must be outside the checkout")
    args.out.mkdir(parents=True, exist_ok=True, mode=0o700)
    if any(args.out.iterdir()):
        parser.error("--out must be empty, to preserve prior evidence")

    initial_digest = digest(args.firmware)
    command = [str(args.emulator), "--chip", "esp32s3",
               "--firmware", str(args.firmware), "--elf", str(args.elf),
               "--psram-size", "8M", "--console", "uart0" if args.uart_port else "usb-jtag",
               "--net", "user,restrict=yes", "--log-color", "never",
               "--i2c-dev", "1:0x3c,width=2,fill=0,reg=0x300a=0x36,reg=0x300b=0x60",
               "--camera-frames", str(args.frames), "--camera-format", "jpeg",
               "--camera-fps", "5", "--camera-mjpeg", str(args.out / "dma.mjpeg"),
               "--log-file", str(args.out / "emulator.log")]
    if args.control_port:
        command += ["--control-tcp", f"127.0.0.1:{args.control_port}"]
    if args.uart_port:
        command += ["--uart-tcp", f"127.0.0.1:{args.uart_port}"]
    # Deliberately omit --save-state. Never import physical-device NVS/eFuses.
    master, slave = pty.openpty()
    tty.setraw(slave)
    port = os.ttyname(slave)
    emulator_socket = None
    if args.uart_port:
        with socket.socket() as check:
            check.bind(("127.0.0.1", args.uart_port))
        emulator_slave = subprocess.DEVNULL
    else:
        emulator_master, emulator_slave = pty.openpty()
        tty.setraw(emulator_slave)
    environment = dict(os.environ, ESP_EMU_DISABLE_AUTOUPDATES="1")
    # Keep terminal line discipline out of the binary transport on both sides.
    # A raw TTY also makes emulator terminal state inspectable during debugging.
    process = subprocess.Popen(command, stdin=emulator_slave,
                               stdout=emulator_slave, stderr=subprocess.PIPE,
                               env=environment, start_new_session=True)
    if args.uart_port:
        deadline = time.monotonic() + 10
        while True:
            try:
                emulator_socket = socket.create_connection(("127.0.0.1", args.uart_port), timeout=0.2)
                break
            except OSError:
                if process.poll() is not None or time.monotonic() >= deadline:
                    process.kill()
                    process.wait(timeout=5)
                    process.stderr.close()
                    os.close(master)
                    os.close(slave)
                    raise RuntimeError("emulator UART listener did not start")
                time.sleep(0.05)
        emulator_master = emulator_socket.fileno()
    else:
        os.close(emulator_slave)
    state = {"emulated": True, "status": "running", "serial_port": port,
             "selector": "wendy-lite:" + port,
             "selector_requires_serial_discovery": True,
             "pid": process.pid, "bridge_pid": os.getpid(),
             "firmware_sha256": initial_digest, "command": command,
             "management_transport": "UART0 via localhost TCP" if args.uart_port else "USB Serial/JTAG",
             "seconds_limit": args.seconds, "peripheral_model": "OV3660 register file and DVP JPEG input"}
    atomic_json(args.out / "selector.json", state)
    print(json.dumps(state), flush=True)
    stopped = False

    def stop(_sig, _frame):
        nonlocal stopped
        stopped = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    started = time.monotonic()
    to_emulator = bytearray()
    to_client = bytearray()
    counts = {"client_to_firmware": 0, "firmware_to_client": 0}
    wire_input = (args.out / "serial-input.bin").open("wb") if args.trace_serial else None
    wire_output = (args.out / "serial-output.bin").open("wb") if args.trace_serial else None
    reason = "duration_limit"
    fds = [master, emulator_master, process.stderr.fileno()]
    for fd in fds:
        os.set_blocking(fd, False)
    try:
        with (args.out / "stderr.log").open("wb") as errors:
            while not stopped and time.monotonic() - started < args.seconds:
                if process.poll() is not None:
                    reason = "emulator_exit"
                    break
                for name, limit in (("stderr.log", MAX_LOG), ("emulator.log", MAX_LOG), ("dma.mjpeg", MAX_DMA), ("serial-input.bin", MAX_LOG), ("serial-output.bin", MAX_LOG)):
                    path = args.out / name
                    if path.exists() and path.stat().st_size >= limit:
                        raise RuntimeError(f"artifact size limit reached: {name}")
                if len(to_emulator) > MAX_QUEUE or len(to_client) > MAX_QUEUE:
                    raise RuntimeError("serial peer did not drain its bounded queue")
                with selectors.DefaultSelector() as sel:
                    sel.register(master, selectors.EVENT_READ | (selectors.EVENT_WRITE if to_client else 0), "client")
                    sel.register(emulator_master, selectors.EVENT_READ | (selectors.EVENT_WRITE if to_emulator else 0), "firmware")
                    sel.register(process.stderr, selectors.EVENT_READ, "errors")
                    for key, events in sel.select(0.1):
                        if key.data == "client":
                            if events & selectors.EVENT_READ:
                                chunk = os.read(master, 65536)
                                counts["client_to_firmware"] += len(chunk)
                                to_emulator.extend(chunk)
                            if events & selectors.EVENT_WRITE:
                                del to_client[:os.write(master, to_client)]
                        elif key.data == "firmware":
                            if events & selectors.EVENT_READ:
                                chunk = os.read(key.fd, 65536)
                                if not chunk:
                                    raise RuntimeError("emulator management transport closed")
                                counts["firmware_to_client"] += len(chunk)
                                to_client.extend(chunk)
                                if wire_output:
                                    wire_output.write(chunk)
                                    wire_output.flush()
                            if events & selectors.EVENT_WRITE:
                                count = os.write(key.fd, to_emulator)
                                if wire_input:
                                    wire_input.write(to_emulator[:count])
                                    wire_input.flush()
                                del to_emulator[:count]
                        elif key.data == "errors":
                            errors.write(os.read(key.fd, 65536))
                            errors.flush()
    except Exception as error:
        reason = str(error)
        raise
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        process.stderr.close()
        if emulator_socket:
            emulator_socket.close()
        else:
            os.close(emulator_master)
        for stream in (wire_input, wire_output):
            if stream:
                stream.close()
        os.close(master)
        os.close(slave)
        unchanged = digest(args.firmware) == initial_digest
        state.update(status="stopped", reason="signal" if stopped else reason,
                     elapsed_seconds=round(time.monotonic() - started, 3),
                     emulator_exit_code=process.returncode,
                     firmware_unchanged=unchanged, byte_counts=counts)
        atomic_json(args.out / "selector.json", state)
        if not unchanged:
            raise RuntimeError("firmware input changed while the emulator ran")
    if reason == "emulator_exit":
        raise RuntimeError(f"emulator exited before the requested duration: {process.returncode}")


if __name__ == "__main__":
    main()
