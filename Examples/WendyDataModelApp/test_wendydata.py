"""Unit tests for the pure parts of the data socket client.

Run with: python3 -m unittest discover Examples/WendyDataModelApp

No camera, model, or agent is required: these cover the record framing
and the uncertainty formula, the two pieces the agent-side smoke tests
cannot see.
"""

import json
import struct
import sys
import threading
import types
import unittest
import unittest.mock

import wendydata

# wendyframes decodes pixels, so it imports cv2 and numpy. Neither is needed by
# the logic under test here (draining and the reconnect budget), and requiring
# them would mean no one runs these tests outside the runtime image. Stub them
# the way the transport dependencies used to be stubbed.
for _name in ("cv2", "numpy"):
    if _name not in sys.modules:
        _stub = types.ModuleType(_name)
        _stub.ndarray = object
        sys.modules[_name] = _stub

import wendyframes


class TestUncertaintyScore(unittest.TestCase):
    def test_no_detections_is_fully_uncertain(self):
        self.assertEqual(wendydata.uncertainty_score([], 0.25), 1.0)

    def test_below_threshold_detections_are_ignored(self):
        self.assertEqual(wendydata.uncertainty_score([0.1, 0.2], 0.25), 1.0)

    def test_uses_max_confidence(self):
        self.assertAlmostEqual(wendydata.uncertainty_score([0.3, 0.9, 0.5], 0.25), 0.1)

    def test_certain_detection_scores_zero(self):
        self.assertEqual(wendydata.uncertainty_score([1.0], 0.25), 0.0)

    def test_clamped_to_unit_interval(self):
        # A confidence above 1.0 (a broken model) must not go negative;
        # campaign model.uncertainty thresholds are defined on 0..1.
        self.assertEqual(wendydata.uncertainty_score([1.5], 0.25), 0.0)

    def test_accepts_generators(self):
        self.assertAlmostEqual(wendydata.uncertainty_score((c for c in [0.75]), 0.25), 0.25)


class TestFraming(unittest.TestCase):
    def test_length_prefix_is_big_endian(self):
        frame = wendydata.frame_record({"version": 1})
        (length,) = struct.unpack(">I", frame[:4])
        self.assertEqual(length, len(frame) - 4)
        self.assertEqual(json.loads(frame[4:]), {"version": 1})

    def test_oversized_record_is_refused(self):
        with self.assertRaises(ValueError):
            wendydata.frame_record({"blob": "x" * wendydata.MAX_RECORD_BYTES})


class TestRecords(unittest.TestCase):
    def test_prediction_record_shape(self):
        record = wendydata.build_prediction(
            "yolov8n", "8.3.63", 0.35, [{"class_name": "person", "confidence": 0.65}]
        )
        self.assertEqual(record["version"], 1)
        self.assertEqual(record["type"], "prediction")
        self.assertEqual(record["model"], "yolov8n")
        self.assertEqual(record["attributes"]["model_version"], "8.3.63")
        # Campaign model.uncertainty triggers read attributes["uncertainty"].
        self.assertAlmostEqual(record["attributes"]["uncertainty"], 0.35)
        self.assertEqual(len(record["attributes"]["detections"]), 1)
        self.assertIn("client_boottime_nanos", record)
        self.assertIn("boot_id", record)

    def test_event_record_shape(self):
        record = wendydata.build_event("person_detected", {"confidence": 0.9})
        self.assertEqual(record["version"], 1)
        self.assertEqual(record["type"], "event")
        self.assertEqual(record["name"], "person_detected")
        self.assertEqual(record["attributes"]["confidence"], 0.9)

    def test_records_fit_the_frame_limit(self):
        record = wendydata.build_prediction("yolov8n", "8.3.63", 1.0, [])
        self.assertLessEqual(len(wendydata.frame_record(record)), wendydata.MAX_RECORD_BYTES)


class TestInputReferences(unittest.TestCase):
    """A prediction must be able to name the harness samples it came from;
    that binding is what lets an episode be replayed as (input, outcome)."""

    def test_prediction_without_inputs_omits_the_field(self):
        record = wendydata.build_prediction("yolov8n", "8.3.63", 1.0, [])
        self.assertNotIn("inputs", record)

    def test_prediction_carries_input_references(self):
        record = wendydata.build_prediction(
            "yolov8n", "8.3.63", 0.4, [], inputs=[{"source_id": "v4l2:/dev/video0", "sample_id": 17}]
        )
        self.assertEqual(record["inputs"], [{"source_id": "v4l2:/dev/video0", "sample_id": 17}])

    def test_input_references_are_normalized(self):
        record = wendydata.build_prediction(
            "yolov8n", "8.3.63", 0.4, [], inputs=[{"source_id": "cam", "sample_id": "5"}]
        )
        self.assertEqual(record["inputs"], [{"source_id": "cam", "sample_id": 5}])

    def test_input_references_are_capped_to_the_agent_limit(self):
        refs = [{"source_id": "cam", "sample_id": i} for i in range(100)]
        record = wendydata.build_prediction("yolov8n", "8.3.63", 0.4, [], inputs=refs)
        self.assertEqual(len(record["inputs"]), wendydata.MAX_INPUT_REFS)
        # The newest references are the ones worth keeping.
        self.assertEqual(record["inputs"][-1]["sample_id"], 99)


class TestPersonAppearance(unittest.TestCase):
    """`person_detected` is a campaign trigger, and the campaign starts one
    episode per trigger. So the event must fire once when a person arrives and
    stay quiet while they remain: level-triggering it would turn one person
    walking past into a stream of episodes."""

    @staticmethod
    def _app():
        """Import app.py with its model and imaging dependencies stubbed.

        The appearance rule needs neither a model nor OpenCV, and a unit test
        must not require onnxruntime to exercise it."""
        for name in ("cv2", "numpy", "onnxruntime"):
            if name not in sys.modules:
                stub = types.ModuleType(name)
                stub.ndarray = object
                sys.modules[name] = stub
        import app

        return app

    @staticmethod
    def _person(confidence):
        return {"class_id": 0, "class_name": "person", "confidence": confidence}

    @staticmethod
    def _chair(confidence=0.7):
        return {"class_id": 56, "class_name": "chair", "confidence": confidence}

    def test_an_empty_frame_reports_nobody(self):
        app = self._app()
        self.assertEqual(app.person_appearance([], False), (False, None))

    def test_other_classes_are_not_people(self):
        app = self._app()
        # The scene this demo runs in is full of furniture; none of it may fire
        # the trigger.
        self.assertEqual(app.person_appearance([self._chair()], False), (False, None))

    def test_arrival_reports_the_best_confidence(self):
        app = self._app()
        present, confidence = app.person_appearance(
            [self._person(0.61), self._chair(), self._person(0.88)], False
        )
        self.assertTrue(present)
        self.assertAlmostEqual(confidence, 0.88)

    def test_staying_in_frame_fires_only_once(self):
        app = self._app()
        present, confidence = app.person_appearance([self._person(0.9)], False)
        self.assertEqual((present, confidence), (True, 0.9))
        # Every later frame of the same appearance stays silent, so the campaign
        # gets one episode covering the person rather than one per frame.
        for _ in range(20):
            present, confidence = app.person_appearance([self._person(0.9)], present)
            self.assertTrue(present)
            self.assertIsNone(confidence)

    def test_leaving_and_returning_fires_again(self):
        app = self._app()
        present, first = app.person_appearance([self._person(0.8)], False)
        self.assertEqual(first, 0.8)
        present, gone = app.person_appearance([], present)
        self.assertEqual((present, gone), (False, None))
        # A second, separate appearance is a second episode, which is correct:
        # it is a different moment worth recording.
        present, again = app.person_appearance([self._person(0.7)], present)
        self.assertEqual((present, again), (True, 0.7))




class _FakeNode:
    """A CameraNode stand-in: a scripted queue of frames and failures.

    Each entry is either a Frame to deliver or an OSError to raise. `ready`
    says how many are already waiting, which is what readable() reports and
    what freshest_frames drains.
    """

    def __init__(self, script, ready=0):
        self.script = list(script)
        self.ready = ready
        self.path = "/dev/videoFake"
        self.source_id = "fake"
        self.closed = 0

    def read(self):
        if not self.script:
            raise OSError("node ended")
        item = self.script.pop(0)
        if isinstance(item, Exception):
            raise item
        if self.ready:
            self.ready -= 1
        return item

    def readable(self):
        return self.ready > 0

    def close(self):
        self.closed += 1


def _frame(boottime):
    return wendyframes.Frame(image=None, source_id="fake", boottime_nanos=boottime)


class TestFreshestFrames(unittest.TestCase):
    """The drain is what keeps predictions on current input; if it silently
    stopped draining, every prediction would still be produced and would still
    reference a real frame, just an increasingly stale one. So assert the
    discard count, not merely that frames arrive."""

    def test_every_frame_is_yielded_when_the_consumer_keeps_up(self):
        node = _FakeNode([_frame(1), _frame(2), _frame(3)], ready=0)
        got = []
        for frame, discarded in wendyframes.freshest_frames(node):
            got.append((frame.boottime_nanos, discarded))
            if len(got) == 3:
                break
        self.assertEqual(got, [(1, 0), (2, 0), (3, 0)])

    def test_a_slow_consumer_gets_the_newest_frame_and_the_discard_count(self):
        # Three already queued: the consumer must be handed the third and told
        # two were dropped, not handed the first.
        node = _FakeNode([_frame(1), _frame(2), _frame(3), _frame(4)], ready=3)
        frame, discarded = next(iter(wendyframes.freshest_frames(node)))
        self.assertEqual((frame.boottime_nanos, discarded), (3, 2))

    def test_a_reader_failure_reaches_the_consumer(self):
        node = _FakeNode([OSError("device gone")])
        with self.assertRaises(OSError):
            next(iter(wendyframes.freshest_frames(node)))


class TestResilientFrames(unittest.TestCase):
    """The reconnect budget must refill on delivered frames, or weeks of
    unrelated agent restarts eventually add up to a shutdown."""

    def setUp(self):
        self.opened = []

    def _patch(self, nodes):
        seq = list(nodes)

        def factory(path, source_id=""):
            if not seq:
                raise OSError("no node")
            item = seq.pop(0)
            if isinstance(item, Exception):
                raise item
            self.opened.append(item)
            return item

        return unittest.mock.patch.object(wendyframes, "CameraNode", factory)

    def test_frames_resume_after_the_node_goes_away(self):
        first = _FakeNode([_frame(1), OSError("node ended")])
        second = _FakeNode([_frame(2)])
        got = []
        with self._patch([first, second]):
            for frame, _ in wendyframes.resilient_frames("/dev/videoFake", "fake", 2, 0):
                got.append(frame.boottime_nanos)
                if len(got) == 2:
                    break
        self.assertEqual(got, [1, 2])
        self.assertEqual(first.closed, 1)

    def test_a_failing_open_is_retried_then_given_up_on(self):
        with self._patch([OSError("a"), OSError("b"), OSError("c")]):
            with self.assertRaises(OSError):
                next(iter(wendyframes.resilient_frames("/dev/videoFake", "fake", 2, 0)))

    def test_zero_attempts_exits_on_the_first_failure(self):
        with self._patch([OSError("gone")]):
            with self.assertRaises(OSError):
                next(iter(wendyframes.resilient_frames("/dev/videoFake", "fake", 0, 0)))

    def test_the_budget_is_refilled_by_every_frame(self):
        # Two failures, a delivered frame, then two more failures. With a
        # budget of 2 that only survives if the frame refilled it.
        nodes = [OSError("1"), OSError("2"), _FakeNode([_frame(7), OSError("end")]),
                 OSError("3"), OSError("4"), _FakeNode([_frame(8)])]
        got = []
        with self._patch(nodes):
            for frame, _ in wendyframes.resilient_frames("/dev/videoFake", "fake", 2, 0):
                got.append(frame.boottime_nanos)
                if len(got) == 2:
                    break
        self.assertEqual(got, [7, 8])
