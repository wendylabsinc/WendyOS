from types import SimpleNamespace
from unittest.mock import Mock, patch
import unittest
from integration.nativesensorfusion import model, scenario


class ScenarioTests(unittest.TestCase):
    def test_terminal_bridge_requires_complete_output_and_successful_child(self):
        aggregate = {'version': 1, 'status': 'complete', 'mode': 'native-bridge'}
        node = SimpleNamespace(python=Mock(side_effect=[
            SimpleNamespace(returncode=0, stdout=model.json_bytes(aggregate)),
            SimpleNamespace(returncode=0, stdout=model.json_bytes({'succeeded': True,
                'exit_code': 0, 'output_truncated': False}))]))
        self.assertEqual(scenario.aggregate(node, '/proofs'), aggregate)
        node.python = Mock(side_effect=[SimpleNamespace(returncode=0, stdout=model.json_bytes(aggregate)),
            SimpleNamespace(returncode=0, stdout=model.json_bytes({'succeeded': False,
                'exit_code': 1, 'output_truncated': False}))])
        with self.assertRaises(model.Refused):
            scenario.aggregate(node, '/proofs')

    def test_native_gate_is_immediately_before_first_single_attempt_step(self):
        events = []
        a = SimpleNamespace(service_volume=lambda s, *args: '/config/'+s,
            write=lambda path, data: events.append(('gate', path)),
            scene_request=Mock(side_effect=model.Refused('uncertain step')))
        runner = SimpleNamespace(nodes={'a': a, 'b': object()},
            step=lambda name: events.append(('phase', name)), save=Mock(),
            inputs={'run_id': 'test', 'anchor_unix_ns': model.ANCHOR})
        with patch.object(scenario, 'wait_value', return_value={}):
            with self.assertRaises(model.Refused):
                scenario.execute(runner)
        self.assertEqual(events, [('phase', 'source-readiness'), ('gate', '/config/camera-b/go'), ('phase', 'sample-1')])
        a.scene_request.assert_called_once_with('/v1/step', {'simulation_time_ns': 1_000_000_000,
                                                          'sensors': list(model.SENSORS)})
