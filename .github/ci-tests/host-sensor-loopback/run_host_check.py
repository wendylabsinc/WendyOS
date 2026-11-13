"""Run the host fixture over authenticated Wendy shell with a bounded deadline."""

from pathlib import Path
import subprocess
import sys


TIMEOUT_SECONDS = 60


def main(argv):
    if len(argv) != 3 or argv[2] not in ("video", "video-audio"):
        print("FAIL: expected Wendy binary, device selector, and loopback scope")
        return 2
    wendy, device, scope = argv
    script = Path(__file__).with_name("check.sh").read_text()
    try:
        result = subprocess.run(
            [wendy, "device", "shell", "--device", device, "--", "sh", "-c", script, "--", scope],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, timeout=TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired as error:
        if error.output:
            print(error.output.decode(errors="replace") if isinstance(error.output, bytes) else error.output)
        print(f"FAIL: device host loopback check exceeded {TIMEOUT_SECONDS}s")
        return 1
    except OSError as error:
        print(f"FAIL: cannot start authenticated device shell: {error}")
        return 1
    print(result.stdout, end="")
    if result.returncode != 0:
        return 1
    # The CLI can receive EOF before the remote exit-code frame. Require the
    # script's final verdict too. splitlines handles PTY CRLF output.
    if f"PASS: host sensor loopback support ({scope})" not in result.stdout.splitlines():
        print("FAIL: host shell returned without a successful loopback verdict")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
