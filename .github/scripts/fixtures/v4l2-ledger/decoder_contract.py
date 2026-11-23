"""Actual H264 reorder/pixel proof; no camera or hardware access."""
from collections import deque
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


def encoded_scenes(width=64, b_frames=2):
    encoder = av.CodecContext.create('libx264', 'w')
    encoder.width, encoder.height = width, 48
    encoder.pix_fmt = 'yuv420p'
    encoder.max_b_frames = b_frames
    encoder.time_base = Fraction(1, 25)
    encoder.framerate = Fraction(25, 1)
    encoder.options = {'crf': '18', 'preset': 'medium', 'x264-params': f'bframes={b_frames}:b-adapt=0:keyint=12:scenecut=0:repeat-headers=1'}
    packets = []
    for index in range(24):
        pixels = np.empty((48, width, 3), np.uint8)
        pixels[:] = [(index % 12) * 17, 20, 220 - (index % 12) * 13]
        frame = av.VideoFrame.from_ndarray(pixels, format='bgr24')
        frame.pts = index
        packets.extend(encoder.encode(frame))
    packets.extend(encoder.encode(None))
    assert len(packets) == 24
    if b_frames:
        assert [packet.pts for packet in packets] != list(range(24)), 'encoder did not exercise reordered output'
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
            assert np.max(np.abs(mean - [(index % 12) * 17, 20, 220 - (index % 12) * 13])) < 8, (index, mean)
            assert decoded.input_refs() == [{'source_id': 'v4l2:fixture', 'sample_id': decoded.sample_id}]
            seen.append(index)
    assert len(seen) >= 8, seen
    assert seen == sorted(seen), seen
    print('H264_IDENTITY_REORDER_VERIFIED', seen)
    late = wendyframes.H264Frames('v4l2:late')
    packets = encoded_scenes()
    key = next(i for i, p in enumerate(packets[1:], 1) if p.is_keyframe)
    recovered = []
    for i, packet in enumerate(packets[1:], 1):
        decoded = late.decode(bytes(packet), 1000 + packet.pts, 2000 + packet.pts, 0)
        if i < key:
            assert not decoded, 'mid-GOP startup emitted undecodable pictures'
        recovered.extend(decoded)
    assert len(recovered) >= 8, 'did not recover at the next IDR'
    assert recovered[0].sample_id == 1000 + packets[key].pts
    assert recovered[0].dropped_before == key - 1
    print('H264_MID_GOP_STARTUP_RECOVERED')
    # Change SPS dimensions after a decoded frame is available. Real decoder
    # reordering can consume the queued packet without returning a picture.
    node = object.__new__(wendyframes.CameraNode)
    node._decoder = wendyframes.H264Frames('v4l2:drain')
    prior = []
    for packet in encoded_scenes(b_frames=0):
        prior.extend(node._decoder.decode(bytes(packet), 3000 + packet.pts, 4000 + packet.pts, 0))
    node._decoded = deque([prior[-1]])
    queued = deque(encoded_scenes(width=80)[:1])
    def consume():
        assert queued, 'drain blocked on an empty encoded-packet queue'
        packet = queued.popleft()
        node._decoded.extend(node._decoder.decode(bytes(packet), 5000 + packet.pts, 6000 + packet.pts, 0))
    node._read_buffer = consume
    node.readable = lambda: bool(node._decoded or queued)
    available, _ = next(wendyframes.freshest_frames(node))
    assert available.sample_id >= 3000
    assert not queued, 'fixture did not reach a no-picture packet after SPS change'
    print('H264_READY_PICTURE_NOT_BLOCKED_BY_REORDER')
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
