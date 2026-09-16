"""Extract payload files from a .wdr export; requires generated Protobuf types.

python3 read_export.py samples.wdr output-directory
Each output file holds the original payload. Metadata is printed separately.
"""
import argparse
import pathlib
import struct
from wendy.agent.apps.v1 import recording_pb2 as pb


def crc32c(data):
    value = 0xFFFFFFFF
    for byte in data:
        value ^= byte
        for _ in range(8):
            value = (value >> 1) ^ (0x82F63B78 if value & 1 else 0)
    return value ^ 0xFFFFFFFF


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("input", type=pathlib.Path)
parser.add_argument("output", type=pathlib.Path)
args = parser.parse_args()
args.output.mkdir(parents=True, exist_ok=False)
with args.input.open("rb") as source:
    index = 0
    while header := source.read(8):
        if len(header) != 8:
            raise ValueError("truncated record header")
        length, checksum = struct.unpack("!II", header)
        if not 0 < length <= 1024 * 1024 + 64 * 1024:
            raise ValueError("invalid record length")
        body = source.read(length)
        if len(body) != length or crc32c(body) != checksum:
            raise ValueError("truncated or corrupt record")
        stored = pb.StoredRecord.FromString(body)
        if not stored.HasField("record"):
            raise ValueError("missing record")
        (args.output / f"{index:08d}.payload").write_bytes(stored.record.payload)
        print(index, stored.app_id, stored.service, stored.stream,
              stored.stream_descriptor.media_type, stored.record.id,
              stored.receipt_boottime_nanos)
        index += 1
