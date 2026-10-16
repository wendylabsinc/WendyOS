"""POSIX native transport: exclusive port ownership, bounded nonblocking I/O."""

import array
import errno
import fcntl
import os
import select
import stat
import struct
import subprocess
import sys
import termios
import time

from protocol import ReaderError, request, response


def check_port_owners(port):
    """Reject visible pre-existing owners, including macOS dial-in/callout aliases.

    Kernel TIOCEXCL prevents later ordinary opens. It alone does not detect a
    process that opened a nonexclusive descriptor before us. lsof adds that check
    on native hosts. Containers only see their PID namespace; preflight on the
    Linux host is still required before granting a serial device to a container.
    """
    paths = [os.path.realpath(port)]
    if sys.platform == "darwin":
        leaf = os.path.basename(paths[0])
        if leaf.startswith(("cu.", "tty.")):
            paths.append("/dev/" + ("tty." if leaf.startswith("cu.") else "cu.") + leaf.split(".", 1)[1])
    try:
        result = subprocess.run(["lsof", "-nP", "-F", "p", "--", *paths], capture_output=True, timeout=5, check=False)
    except subprocess.TimeoutExpired as exc:
        raise ReaderError("ownership_unknown", "Port-owner check timed out; ownership is unknown, retry when the host is responsive") from exc
    except OSError as exc:
        raise ReaderError("ownership_unknown", "Cannot check port owners; install lsof and retry") from exc
    if result.returncode not in {0, 1}:
        raise ReaderError("ownership_unknown", "lsof could not check port owners")
    owners = {int(line[1:]) for line in result.stdout.decode(errors="replace").splitlines()
              if line.startswith("p") and line[1:].isdigit()} - {os.getpid()}
    if owners:
        raise ReaderError("busy", "Serial port is already owned by another process")


def io_error(exc):
    # Linux termios.error carries errno in args; it is not always an OSError.
    number = getattr(exc, "errno", exc.args[0] if exc.args else None)
    code = {errno.EACCES: "permission_denied", errno.EPERM: "permission_denied",
            errno.EBUSY: "busy", errno.EAGAIN: "busy", errno.ENOENT: "disconnected",
            errno.ENODEV: "disconnected", errno.ENXIO: "disconnected",
            errno.EIO: "disconnected"}.get(number, "serial_error")
    return ReaderError(code, str(exc))


class SerialTransport:
    def __init__(self, port, baud, timeout, stop):
        self.fd = None
        self.original = None
        self.claimed = False
        self.stop = stop
        self.timeout = timeout
        try:
            # Do not configure a regular file or accept a symlink to one.
            if not stat.S_ISCHR(os.stat(port).st_mode):
                raise ReaderError("invalid_port", "Serial port must be a character device")
            check_port_owners(port)
            if stop.is_set():
                raise ReaderError("stopping", "Reader stopped before claiming serial port")
            self.fd = os.open(port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
            fcntl.flock(self.fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            fcntl.ioctl(self.fd, termios.TIOCEXCL)
            self.claimed = True
            check_port_owners(port)
            if stop.is_set():
                raise ReaderError("stopping", "Reader stopped before configuring serial port")
            self.original = termios.tcgetattr(self.fd)
            settings = termios.tcgetattr(self.fd)
            settings[0] = settings[1] = settings[3] = 0
            settings[2] &= ~(termios.CSIZE | termios.PARENB | termios.CSTOPB | termios.HUPCL
                             | getattr(termios, "CRTSCTS", 0))
            settings[2] |= termios.CS8 | termios.CLOCAL | termios.CREAD
            speed = getattr(termios, "B" + str(baud), None)
            if sys.platform == "darwin":
                speed = termios.B9600
            if speed is None:
                raise ReaderError("unsupported_baud", "Baud rate unavailable on this host")
            settings[4] = settings[5] = speed
            settings[6][termios.VMIN] = settings[6][termios.VTIME] = 0
            termios.tcsetattr(self.fd, termios.TCSANOW, settings)
            if sys.platform == "darwin":
                # IOKit/serial/ioss.h _IOW('T', 2, speed_t), speed_t is unsigned long.
                value = array.array("L", [baud])
                fcntl.ioctl(self.fd, 0x80000000 | (value.itemsize << 16) | (ord("T") << 8) | 2, value)
        except (OSError, termios.error) as exc:
            self.close()
            raise io_error(exc) from exc
        except Exception:
            self.close()
            raise

    def close(self):
        if self.fd is None:
            return
        try:
            if self.original is not None:
                try:
                    termios.tcsetattr(self.fd, termios.TCSANOW, self.original)
                except (OSError, termios.error):
                    pass
            # Release TIOCEXCL explicitly (PTY tests may keep another descriptor).
            if self.claimed:
                try:
                    fcntl.ioctl(self.fd, termios.TIOCNXCL)
                except OSError:
                    pass
        finally:
            os.close(self.fd)
            self.fd = None

    def read(self, motor_id, address, count):
        # Construct bytes here, never accept arbitrary packets from callers.
        packet = request(motor_id, address, count)
        deadline = time.monotonic() + self.timeout
        received = bytearray()
        sent = 0
        try:
            termios.tcflush(self.fd, termios.TCIFLUSH)
            while time.monotonic() < deadline:
                if self.stop.is_set():
                    raise ReaderError("stopped", "Reader stopping")
                readable, writable, exceptional = select.select(
                    [self.fd] if sent == len(packet) else [],
                    [self.fd] if sent < len(packet) else [], [self.fd],
                    min(0.02, max(0, deadline - time.monotonic())))
                if exceptional:
                    raise ReaderError("disconnected", "Serial device disconnected")
                if writable:
                    try:
                        sent += os.write(self.fd, packet[sent:])
                    except (BlockingIOError, InterruptedError):
                        continue
                if readable:
                    try:
                        data = os.read(self.fd, 256)
                    except (BlockingIOError, InterruptedError):
                        continue
                    if not data:
                        raise ReaderError("disconnected", "Serial device closed")
                    received.extend(data)
                while len(received) >= 4:
                    if received[:2] != b"\xff\xff":
                        del received[0]
                        continue
                    length = received[3] + 4
                    if not 6 <= length <= 32:
                        raise ReaderError("protocol", "Response length outside bounded frame size")
                    if len(received) < length:
                        break
                    frame = bytes(received[:length])
                    del received[:length]
                    # An unexpected ID is a bus error, never another joint's reading.
                    return response(frame, motor_id, count)
            raise ReaderError("timeout", "No valid response; possible unpowered arm, wrong port/baud/ID or cable")
        except (OSError, termios.error) as exc:
            raise io_error(exc) from exc


class MockTransport:
    """Deterministic synthetic register bytes, including optional partial failures."""

    def __init__(self, failed_ids=()):
        self.failed_ids = failed_ids

    def read(self, motor_id, address, count):
        request(motor_id, address, count)
        if motor_id in self.failed_ids:
            raise ReaderError("timeout", "Synthetic missing motor (mock transport)")
        if address == 3:
            return struct.pack("<H", 777), 0
        return struct.pack("<H", 1000 + motor_id * 100) + b"\x00\x00\x00\x00\x78\x1e", 0

    def close(self):
        pass
