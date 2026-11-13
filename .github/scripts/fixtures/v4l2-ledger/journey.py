"""Generated pixels and a tiny ONNX model; actual example I/O, no model-accuracy claim."""
import argparse
from fractions import Fraction
import hashlib
import json
import os
from pathlib import Path
import sys
import time

import av
import numpy as np
import onnx
from onnx import helper, numpy_helper, TensorProto
import onnxruntime as ort

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / 'Examples/WendyDataModelApp'))
import app
import wendydata
import wendyframes


def prepare(output):
    output.mkdir(parents=True, exist_ok=False)
    for name, rgb in [('red', (235, 20, 20)), ('blue', (20, 20, 235))]:
        codec = av.CodecContext.create('libx264', 'w')
        codec.width, codec.height, codec.pix_fmt = 64, 48, 'yuv420p'
        codec.time_base = Fraction(1, 25)
        codec.options = {'preset': 'ultrafast', 'tune': 'zerolatency', 'crf': '18', 'x264-params': 'keyint=1:repeat-headers=1'}
        pixels = np.full((48, 64, 3), rgb, np.uint8)
        packets = list(codec.encode(av.VideoFrame.from_ndarray(pixels, format='rgb24')))
        packets += list(codec.encode(None))
        assert len(packets) == 1
        (output / (name + '.h264')).write_bytes(bytes(packets[0]))
    # A real, deliberately synthetic RGB-mean model with YOLO's output shape.
    # Red pixels produce a person score; blue pixels produce no detection.
    fixed = np.zeros((1, 84, 1), np.float32)
    fixed[0, :4, 0] = [320, 320, 160, 160]
    basis = np.zeros_like(fixed); basis[0, 4, 0] = 1
    graph = helper.make_graph([
        helper.make_node('Gather', ['pixels', 'red_channel'], ['red'], axis=1),
        helper.make_node('ReduceMean', ['red'], ['score'], axes=[0, 1, 2], keepdims=0),
        helper.make_node('Mul', ['score', 'basis'], ['scores']),
        helper.make_node('Add', ['scores', 'fixed'], ['detections']),
    ], 'synthetic-color', [helper.make_tensor_value_info('pixels', TensorProto.FLOAT, [1, 3, 640, 640])],
       [helper.make_tensor_value_info('detections', TensorProto.FLOAT, [1, 84, 1])],
       [numpy_helper.from_array(np.array(0, np.int64), 'red_channel'), numpy_helper.from_array(basis, 'basis'), numpy_helper.from_array(fixed, 'fixed')])
    model = helper.make_model(graph, opset_imports=[helper.make_opsetid('', 12)], ir_version=10)
    onnx.checker.check_model(model)
    onnx.save(model, output / 'model.onnx')
    (output / 'files.json').write_text(json.dumps({p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in output.iterdir() if p.is_file()}))


def consume(args):
    deadline = time.monotonic() + 5
    while not args.permit.exists():
        if time.monotonic() > deadline: raise TimeoutError('owned process permit missing')
        time.sleep(.01)
    assert args.permit.read_text() == str(os.getpid())
    session = ort.InferenceSession(str(args.fixture / 'model.onnx'), providers=['CPUExecutionProvider'])
    rows = []
    client = wendydata.DataSocketClient(str(args.socket))
    try:
        with wendyframes.CameraNode(str(args.node), args.source) as node:
            for _ in range(12):
                frame = node.read()
                detections = app.detect(session, session.get_inputs()[0].name, frame.image)
                record = wendydata.build_prediction('synthetic-color', 'v1', wendydata.uncertainty_score((d['confidence'] for d in detections), app.CONFIDENCE_THRESHOLD), detections,
                    {'frame_width': frame.image.shape[1], 'frame_height': frame.image.shape[0], 'frame_read_boottime_nanos': frame.boottime_nanos}, inputs=frame.input_refs())
                assert client.send(record) == 'recorded'
                rows.append({'sample_id': frame.sample_id, 'sha256': hashlib.sha256(frame.image.tobytes()).hexdigest(), 'positive': bool(detections), 'receipt': frame.boottime_nanos})
                time.sleep(.065)  # Let some kernel frames pass unscored; identity must still join.
    finally:
        client.close()
    assert len({r['sample_id'] for r in rows}) >= 8
    assert {r['positive'] for r in rows} == {True, False}
    args.report.write_text(json.dumps(rows))
    print('V4L2_EXAMPLE_PREDICTIONS_RECORDED', len(rows))


def verify(args):
    rows = json.loads(args.report.read_text())
    indexes = list(args.episode.glob('cameras/*/index.jsonl'))
    assert len(indexes) == 1
    index = {record['sample_id']: record for record in map(json.loads, indexes[0].read_text().splitlines())}
    outcomes = [json.loads(line) for line in (args.episode / 'events.jsonl').read_text().splitlines()]
    predictions = [record for record in outcomes if record.get('type') == 'prediction' and record.get('app_id') == 'sh.wendy.sample-ledger']
    assert len(predictions) == len(rows), (len(predictions), len(rows))
    by_id = {record['inputs'][0]['sample_id']: record for record in predictions}
    for row in rows:
        identity = row['sample_id']
        assert identity in index, f'V4L2 sample identity does not resolve in recorded camera index: {identity}'
        assert identity in by_id
        prediction = by_id[identity]
        assert prediction['inputs'] == [{'source_id': args.source, 'sample_id': identity}]
        entry = index[identity]
        payload = (args.episode / entry['segment']).read_bytes()[entry['byte_offset']:entry['byte_offset'] + entry['byte_size']]
        decoder = av.CodecContext.create('h264', 'r')
        decoded = decoder.decode(av.Packet(payload)) + decoder.decode(None)
        assert len(decoded) == 1
        pixels = decoded[0].to_ndarray(format='bgr24')
        assert hashlib.sha256(pixels.tobytes()).hexdigest() == row['sha256'], f'prediction/capture pixel mismatch for sample {identity}'
        assert prediction['attributes']['frame_read_boottime_nanos'] == row['receipt']
        assert row['receipt'] != identity, 'receipt time accidentally substituted for sample identity'
        assert bool(prediction['attributes']['detections']) == row['positive']
    print('V4L2_SAMPLE_CAMERA_INDEX_JOIN_VERIFIED', len(rows))
    print('APPLICATION_MODEL_INPUT_DELIVERY_LEDGER_NOT_CLAIMED')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['prepare', 'consume', 'verify'])
    for name in ('fixture', 'node', 'socket', 'report', 'episode', 'permit'): parser.add_argument('--' + name, type=Path)
    parser.add_argument('--source')
    args = parser.parse_args()
    {'prepare': lambda: prepare(args.fixture), 'consume': lambda: consume(args), 'verify': lambda: verify(args)}[args.mode]()
