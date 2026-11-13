import unittest
from r2_sim.runtime import Runtime


class AppControlTests(unittest.TestCase):
    def test_exclusive_owner_watchdog_stop_and_rearm(self):
        r = Runtime()
        token = r.claim_app()['token']
        with self.assertRaises(ValueError):
            r.claim_app()
        command = dict(token=token, sequence=1, speed=.6, steering=.2)
        r.app_command(command)
        r.tick(.1, r.deadline-.1)
        self.assertGreater(r.sim.speed, 0)
        with self.assertRaises(ValueError):
            r.app_command(command)
        r.tick(.01, r.deadline+.001)
        self.assertEqual(r.sim.speed, 0)
        r.revoke()
        with self.assertRaises(ValueError):
            r.app_command({**command,'sequence':2})
        with self.assertRaises(ValueError):
            r.claim_app()
        r.arm('app')
        self.assertNotEqual(token,r.claim_app()['token'])

    def test_browser_pause_reset_block_app_commands(self):
        r=Runtime()
        r.arm('browser')
        with self.assertRaises(ValueError):
            r.claim_app()
        r.arm('app')
        token=r.claim_app()['token']
        r.reset()
        with self.assertRaises(ValueError):
            r.app_command(dict(token=token,sequence=1,speed=.6,steering=0))
        r.paused=True
        with self.assertRaises(ValueError):
            r.arm('app')

    def test_stop_is_immediate_and_preserves_app_session(self):
        r=Runtime()
        token=r.claim_app()['token']
        r.app_command(dict(token=token,sequence=1,speed=1.,steering=0.))
        r.tick(.1,r.deadline-.1)
        r.app_command(dict(token=token,sequence=2,speed=0.,steering=0.,stop=True))
        self.assertEqual(r.sim.speed,0)
        r.app_command(dict(token=token,sequence=3,speed=-.3,steering=0.))
        r.tick(.1,r.deadline-.1)
        self.assertLess(r.sim.speed,0)
