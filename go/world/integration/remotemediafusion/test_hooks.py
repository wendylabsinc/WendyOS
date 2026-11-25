"""Default-preserving configuration and observation extension points."""
import unittest
from unittest.mock import Mock

from integration.remotemediafusion import run, model
from integration.hazard.test_model import identity_fixture
from integration.people.test_model import pair_fixture


class HookTests(unittest.TestCase):
    def test_default_config_hook_equals_original_builders(self):
        owner = object.__new__(run.Runner)
        owner.pair, _ = pair_fixture(); owner.scene = identity_fixture()
        owner.inputs = {'run_id': owner.scene['run_id']}
        owner.scene_url, owner.remote_scene_url = 'http://10.1.0.1:8091', 'http://10.1.0.1:8092'
        for node, services in model.SERVICES.items():
            for service in services:
                if service == 'scene': continue
                if service in model.SENSORS:
                    want = model.capture_config(service, owner.inputs['run_id'], owner.scene,
                        owner.scene_url if node == 'a' else owner.remote_scene_url)
                else:
                    builder = model.fusion_config if service == 'fusion' else model.reader_config
                    want = builder(owner.pair, owner.inputs['run_id'], owner.scene, 9)
                self.assertEqual(owner.service_config(node, service, {'generation': 9}), want)

    def test_custom_observation_retains_success_and_cleanup_result(self):
        owner = object.__new__(run.Runner)
        for name in ('setup', 'enroll', 'projects', 'source_scene', 'commission', 'deploy'):
            setattr(owner, name, Mock())
        owner.observe = Mock(return_value={'custom': True})
        owner.cleanup = Mock(return_value={'complete': True})
        owner.save = Mock()
        result = owner.execute()
        self.assertTrue(result['passed'])
        self.assertEqual(result['observations'], {'custom': True})
        owner.observe.assert_called_once(); owner.cleanup.assert_called_once()


if __name__ == '__main__': unittest.main()
