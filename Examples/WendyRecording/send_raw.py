"""Send one opaque record using only the Python standard library.

Run inside an entitled app: python3 send_raw.py notes < message.txt
A successful send is NOT a durable acknowledgement.
"""
import argparse
import os
import socket
import sys

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("stream", choices=["notes", "temperature"])
args = parser.parse_args()
payload = sys.stdin.buffer.read(1024 * 1024 + 1)
if not 0 < len(payload) <= 1024 * 1024:
    parser.error("provide 1..1048576 bytes on stdin")

with socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET) as sock:
    sock.settimeout(5)
    sock.connect(os.path.join(os.environ["WENDY_DATA_STREAM_DIR"], args.stream + ".sock"))
    if sock.send(payload) != len(payload):
        raise RuntimeError("incomplete packet send")
