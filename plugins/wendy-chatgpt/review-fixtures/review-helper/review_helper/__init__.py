"""Harmless stdout-only review fixture. All records are synthetic."""

from datetime import datetime, timezone
import json
import signal
from threading import Event

APP_ID = 'review-helper'
EMIT_WARNINGS = False
stop = Event()


def emit(level, message, sequence):
    print(json.dumps({
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "level": level,
        "app": APP_ID,
        "synthetic": True,
        "sequence": sequence,
        "message": message,
    }), flush=True)


def main():
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    sequence = 0
    emit("INFO", "Synthetic review fixture ready; stdout only", sequence)
    if EMIT_WARNINGS:
        emit("WARN", "Synthetic review warning; no device fault or action required", sequence)
    while not stop.wait(5):
        sequence += 1
        emit("INFO", "Synthetic heartbeat", sequence)
        if EMIT_WARNINGS and sequence % 3 == 0:
            emit("WARN", "Synthetic review warning; no device fault or action required", sequence)
    emit("INFO", "Synthetic fixture stopped cleanly", sequence)


if __name__ == "__main__":
    main()
