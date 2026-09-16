"""Send one durable time-series batch using generated recording.proto types.

Generate/install dependencies as described in README.md. Keep the same Record
bytes and ID when retrying; do not rebuild it with a new timestamp or payload.
"""
import os
import socket
import time
import uuid
from wendy.agent.apps.v1 import recording_pb2 as pb

with open("/proc/sys/kernel/random/boot_id", encoding="ascii") as f:
    boot_id = f.read().strip()
# Illustrative previously sampled values, spaced one millisecond apart.
start = time.clock_gettime_ns(time.CLOCK_BOOTTIME) - 2_000_000
batch = pb.TimeSeriesBatch(
    timing=pb.SampleTiming(
        clock="CLOCK_BOOTTIME", boot_id=boot_id, count=3,
        start_nanos=start, period_nanos=1_000_000,
    ),
    columns=[pb.SampleColumn(float64_values=pb.Float64Values(values=[21.5, 21.6, 21.4]))],
)
record = pb.Record(
    id=str(uuid.uuid4()), payload=batch.SerializeToString(),
    client_boottime_nanos=start, boot_id=boot_id, timing=batch.timing,
)
encoded = record.SerializeToString()
path = os.path.join(os.environ["WENDY_DATA_STREAM_DIR"], "samples.sock")
for attempt in range(2):
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as sock:
            sock.settimeout(5)
            sock.connect(path)
            if sock.send(encoded) != len(encoded):
                raise ConnectionError("incomplete packet send")
            response = sock.recv(4096)
            if not response:
                raise ConnectionError("socket closed before acknowledgement")
            ack = pb.Ack.FromString(response)
            if ack.id != record.id:
                raise RuntimeError("acknowledgement ID mismatch")
            if ack.state not in (pb.Ack.COMMITTED, pb.Ack.DUPLICATE):
                raise RuntimeError(ack.error or "record rejected")
            print("Durably recorded:", ack.id)
            break
    except OSError:
        if attempt:
            raise
