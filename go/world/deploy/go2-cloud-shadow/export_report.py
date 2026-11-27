"""Read bounded report chunks for explicit export through public container_exec."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import stat

MAX_REPORT = 48 << 20
CHUNK = 128 << 10


def read(capture, offset=None):
    if re.fullmatch(r'[a-z0-9][a-z0-9-]{0,63}', capture) is None:
        raise ValueError('capture_name')
    path = Path('/tmp')/('world-cloud-shadow-'+capture+'.json')
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    try:
        s = os.fstat(fd)
        if not stat.S_ISREG(s.st_mode) or not 0 < s.st_size <= MAX_REPORT:
            raise ValueError('report_bound')
        identity = [s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns]
        with os.fdopen(fd, 'rb', closefd=False) as stream:
            if offset is None:
                h = hashlib.sha256()
                total = 0
                while True:
                    raw = stream.read(CHUNK)
                    if not raw: break
                    total += len(raw)
                    if total > MAX_REPORT: raise ValueError('report_grew')
                    h.update(raw)
                if total != s.st_size: raise ValueError('report_size_changed')
                result = {'bytes': total, 'sha256': h.hexdigest()}
            else:
                if type(offset) is not int or not 0 <= offset < s.st_size:
                    raise ValueError('offset')
                stream.seek(offset)
                raw = stream.read(min(CHUNK, s.st_size-offset))
                if len(raw) != min(CHUNK, s.st_size-offset): raise ValueError('report_short_read')
                result = {'offset': offset, 'bytes': len(raw), 'sha256': hashlib.sha256(raw).hexdigest(),
                          'base64': base64.b64encode(raw).decode('ascii')}
        after = os.fstat(fd)
        if identity != [after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns]:
            raise ValueError('report_changed')
        result['identity'] = identity
        return result
    finally:
        os.close(fd)


if __name__ == '__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--capture',required=True)
    parser.add_argument('--offset',type=int)
    args=parser.parse_args()
    print(json.dumps(read(args.capture,args.offset),sort_keys=True,separators=(',',':')))
