"""Actual H264 reorder/pixel proof; no camera or hardware access."""
from fractions import Fraction
from pathlib import Path
import sys
import argparse
import importlib.util
import av
import numpy as np

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / 'Examples/WendyDataModelApp'))
import wendyframes


def encoded_scenes():
    encoder = av.CodecContext.create('libx264', 'w')
    encoder.width, encoder.height = 64, 48
    encoder.pix_fmt = 'yuv420p'
    encoder.max_b_frames = 2
    encoder.time_base = Fraction(1, 25)
    encoder.framerate = Fraction(25, 1)
    encoder.options = {'crf': '18', 'preset': 'medium', 'x264-params': 'bframes=2:b-adapt=0:keyint=12:scenecut=0:repeat-headers=1'}
    packets = []
    for index in range(12):
        pixels = np.empty((48, 64, 3), np.uint8)
        pixels[:] = [index * 17, 20, 220 - index * 13]
        frame = av.VideoFrame.from_ndarray(pixels, format='bgr24')
        frame.pts = index
        packets.extend(encoder.encode(frame))
    packets.extend(encoder.encode(None))
    assert len(packets) == 12
    assert [packet.pts for packet in packets] != list(range(12)), 'encoder did not exercise reordered output'
    return packets


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--reader', type=Path)
    args = parser.parse_args()
    global wendyframes
    if args.reader:
        spec = importlib.util.spec_from_file_location('controlled_wendyframes', args.reader)
        wendyframes = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = wendyframes
        spec.loader.exec_module(wendyframes)
    decoder = wendyframes.H264Frames('v4l2:fixture')
    seen = []
    for packet in encoded_scenes():
        identity = (1 << 63) + 1234567 + packet.pts
        receipt = 987000000000 + packet.pts
        for decoded in decoder.decode(bytes(packet), identity, receipt, 0):
            index = decoded.sample_id - ((1 << 63) + 1234567)
            assert decoded.boottime_nanos == 987000000000 + index
            mean = decoded.image.mean(axis=(0, 1))
            assert np.max(np.abs(mean - [index * 17, 20, 220 - index * 13])) < 8, (index, mean)
            assert decoded.input_refs() == [{'source_id': 'v4l2:fixture', 'sample_id': decoded.sample_id}]
            seen.append(index)
    assert len(seen) >= 8, seen
    assert seen == sorted(seen), seen
    print('H264_IDENTITY_REORDER_VERIFIED', seen)
    invalid = wendyframes.H264Frames('v4l2:fixture')
    try:
        invalid.decode(b'not an H264 access unit', 42, 87, 0)
    except RuntimeError as error:
        assert 'trustworthy identity' in str(error)
    else:
        raise AssertionError('invalid compressed input was accepted')
    print('H264_MALFORMED_INPUT_REFUSED')


if __name__ == '__main__':
    main()
