"""Compare the actual example reader with a C binary built from Linux headers."""
import ctypes
import json
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[4] / 'Examples/WendyDataModelApp'))
import wendyframes as reader

expected = json.loads(subprocess.run([sys.argv[1]], check=True, capture_output=True, text=True, timeout=3).stdout)
actual = {
    'format_size': ctypes.sizeof(reader._Format),
    'format_union_offset': reader._Format.fmt.offset,
    'buffer_size': ctypes.sizeof(reader._Buffer),
    'buffer_memory_offset': reader._Buffer.offset.offset,
    'buffer_length_offset': reader._Buffer.length.offset,
    'buffer_request_offset': reader._Buffer.request_fd.offset,
    'g_fmt': reader.VIDIOC_G_FMT, 'querybuf': reader.VIDIOC_QUERYBUF, 'dqbuf': reader.VIDIOC_DQBUF,
}
assert actual == expected, {'python': actual, 'linux_headers': expected}
print('LINUX_HEADER_V4L2_ABI_VERIFIED', json.dumps(actual, sort_keys=True))
