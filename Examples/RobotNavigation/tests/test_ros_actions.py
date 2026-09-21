import types
import unittest
from robot_navigation.ros import NavigationActions, Nav2LifecycleMonitor, submit_approach, sync_targets


class Future:
    def __init__(self):
        self.callbacks = []
        self.value = self.error = None
        self.done = self.cancelled = False
    def add_done_callback(self, callback):
        self.callbacks.append(callback)
        if self.done:
            callback(self)
    def complete(self, value=None, error=None):
        self.value, self.error, self.done = value, error, True
        for callback in self.callbacks:
            callback(self)
    def result(self):
        if self.error:
            raise self.error
        return self.value
    def cancel(self):
        self.cancelled = True


class Runtime:
    def __init__(self):
        self.current = {"goal_id": "a", "state": "submitting"}
        self.events = []
    def status(self):
        return {"active_goal": self.current}
    def backend_result(self, goal_id, outcome):
        self.events.append(("result", goal_id, outcome))
        self.current["state"] = "stopping"
    def backend_settled(self, goal_id):
        self.events.append(("settled", goal_id))
    def backend_accepted(self, goal_id):
        if self.current["goal_id"] == goal_id and self.current["state"] == "submitting":
            self.events.append(("accepted", goal_id))
            self.current["state"] = "running"
    def backend_uncertain(self, goal_id, reason):
        self.events.append(("uncertain", goal_id, reason))
        self.current["state"] = "stopping"


class Client:
    def __init__(self):
        self.sent = []
        self.future = Future()
    def server_is_ready(self):
        return True
    def send_goal_async(self, goal, feedback_callback):
        self.sent.append(goal)
        return self.future


class Handle:
    accepted = True
    def __init__(self):
        self.results = []
        self.cancel_count = 0
    def get_result_async(self):
        future = Future()
        self.results.append(future)
        return future
    def cancel_goal_async(self):
        self.cancel_count += 1
        future = Future()
        future.complete(types.SimpleNamespace(return_code=0))
        return future


class ActionTests(unittest.TestCase):
    def setUp(self):
        self.now = 100
        self.runtime, self.client, self.handle = Runtime(), Client(), Handle()
        self.actions = NavigationActions(self.runtime, self.client, lambda pose:pose,
                                         lambda *args:None, clock=lambda:self.now)
    def send(self):
        self.actions.send("a", {"x":1}, .2)
    def accept(self):
        self.client.future.complete(self.handle)

    def test_motion_requires_accepted_current_handle(self):
        self.send()
        self.assertFalse(self.actions.allows_motion())
        self.accept()
        self.assertTrue(self.actions.allows_motion())
        self.runtime.current["state"] = "stopping"
        self.assertFalse(self.actions.allows_motion())

    def test_late_acceptance_after_cancel_never_reenables(self):
        self.send()
        self.runtime.current["state"] = "stopping"
        self.actions.cancel("a")
        self.accept()
        self.assertEqual(self.handle.cancel_count, 1)
        self.assertFalse(self.actions.allows_motion())
        self.assertNotIn(("accepted", "a"), self.runtime.events)
        self.assertFalse(any(e[0] == "result" for e in self.runtime.events))
        self.handle.results[-1].complete(types.SimpleNamespace(status=5))
        self.assertIn(("result", "a", "cancelled"), self.runtime.events)
        self.actions.cancel("a")  # Runtime's terminal-stop cleanup must not leave a ghost.
        self.assertEqual(self.actions.snapshot()["pending_cancellation"], [])

    def test_cancellation_before_dispatch_is_authoritatively_unsent(self):
        self.runtime.current["state"] = "stopping"
        self.actions.cancel("a")
        self.assertEqual(self.client.sent, [])
        self.assertIn(("settled", "a"), self.runtime.events)
        self.assertEqual(self.actions.snapshot()["pending_cancellation"], [])

    def test_failed_acceptance_stays_uncertain_and_blocks_another_send(self):
        self.send()
        self.client.future.complete(error=RuntimeError("connection lost"))
        self.assertEqual(self.actions.snapshot()["pending_submissions"], ["a"])
        self.assertFalse(any(e[0] == "result" for e in self.runtime.events))
        self.runtime.current = {"goal_id":"b", "state":"submitting"}
        self.actions.send("b", {"x":2}, .2)
        self.assertEqual(len(self.client.sent), 1)
        self.assertFalse(self.actions.allows_motion())

    def test_result_transport_failure_retries_without_claiming_terminal(self):
        self.send()
        self.accept()
        old = self.handle.results[-1]
        old.complete(error=RuntimeError("result response lost"))
        self.assertEqual(self.actions.snapshot()["active_handles"], ["a"])
        self.assertFalse(any(e[0] == "result" for e in self.runtime.events))
        self.assertFalse(self.actions.allows_motion())
        self.now += .51
        self.actions.retry()
        self.handle.results[-1].complete(types.SimpleNamespace(status=5))
        self.assertIn(("result", "a", "cancelled"), self.runtime.events)
        old.complete(error=RuntimeError("late duplicate callback"))
        self.assertEqual(self.actions.snapshot()["uncertain_goals"], {})

    def test_nonterminal_result_is_not_treated_as_a_failed_finished_goal(self):
        self.send()
        self.accept()
        self.handle.results[-1].complete(types.SimpleNamespace(status=2))
        self.assertFalse(any(e[0] == "result" for e in self.runtime.events))
        self.assertEqual(self.actions.snapshot()["active_handles"], ["a"])

    def test_hung_cancellation_requests_are_bounded_and_old_callbacks_fenced(self):
        pending = []
        def cancel_async():
            future = Future()
            pending.append(future)
            return future
        self.handle.cancel_goal_async = cancel_async
        self.send()
        self.accept()
        self.runtime.current["state"] = "stopping"
        self.actions.cancel("a")
        self.now += .1
        self.actions.cancel("a")
        self.assertEqual(len(pending), 1)
        for _ in range(20):
            self.now += .51
            self.actions.retry()
            self.assertEqual(len(self.actions.cancel_pending), 1)
            self.assertTrue(all(f.cancelled for f in pending[:-1]))
        pending[0].complete(error=RuntimeError("late cancelled response"))
        self.assertEqual(self.actions.snapshot()["uncertain_goals"], {})
        self.handle.results[-1].complete(types.SimpleNamespace(status=5))
        self.assertTrue(pending[-1].cancelled)
        self.assertEqual(self.actions.cancel_pending, {})


class LifecycleClient:
    def __init__(self):
        self.requests = []
        self.ready = True
    def service_is_ready(self): return self.ready
    def call_async(self, request):
        future = Future()
        self.requests.append(future)
        return future
    def remove_pending_request(self, future): pass


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.now = 100
        self.clients = {name:LifecycleClient() for name in ("planner", "controller", "navigator")}
        self.monitor = Nav2LifecycleMonitor(self.clients, object, clock=lambda:self.now)
    def respond(self, state=3):
        for client in self.clients.values():
            client.requests[-1].complete(types.SimpleNamespace(current_state=types.SimpleNamespace(id=state)))
    def test_all_three_lifecycle_nodes_must_be_fresh_and_active(self):
        self.monitor.poll()
        self.assertFalse(self.monitor.ready())
        self.respond()
        self.assertTrue(self.monitor.ready())
        self.now += .51
        self.assertFalse(self.monitor.ready())
        self.monitor.poll()
        self.respond(2)
        self.assertFalse(self.monitor.ready())
    def test_hung_and_late_responses_cannot_restore_readiness(self):
        self.monitor.poll()
        old = self.clients["planner"].requests[-1]
        self.now += .31
        self.monitor.poll()
        self.assertTrue(old.cancelled)
        old.complete(types.SimpleNamespace(current_state=types.SimpleNamespace(id=3)))
        self.assertIsNone(self.monitor.snapshot()["planner"]["state_id"])
        self.assertFalse(self.monitor.ready())


class ApproachTests(unittest.TestCase):
    def test_explicit_target_loss_invalidates_active_target_immediately(self):
        updates = []
        runtime = types.SimpleNamespace(
            status=lambda:{"active_goal":{"target":{"target_id":"p", "x":3, "y":4}}},
            update_target=lambda *args,**kwargs:updates.append((args,kwargs)))
        registry = types.SimpleNamespace(observed_targets=lambda now:[],
            status=lambda target_id,now:{"status":"lost"})
        sync_targets(runtime, registry, 100.1, "map")
        self.assertEqual(updates, [(("p", 100.1, 3, 4, "map"), {"valid":False})])

    def test_accepted_retry_uses_receipt_without_regrounding_or_current_pose(self):
        metadata={"kind":"approach_person", "target_id":"p", "standoff":1.5,
                  "max_speed":.2, "lease_seconds":10}
        previous={"goal_id":"a", "metadata":metadata}
        runtime=types.SimpleNamespace(request_status=lambda request_id:previous)
        result=submit_approach(runtime, None, None, 200, "r", "p", 1.5, .2, 10)
        self.assertEqual(result["goal"], previous)
        with self.assertRaisesRegex(ValueError, "different approach"):
            submit_approach(runtime, None, None, 200, "r", "other", 1.5, .2, 10)
    def test_no_goal_choice_is_explicitly_not_an_accepted_request(self):
        runtime=types.SimpleNamespace(request_status=lambda request_id:None)
        registry=types.SimpleNamespace(choose_goal=lambda *args,**kwargs:{"goal":None, "status":"already_within_standoff"})
        with self.assertRaisesRegex(ValueError,"no goal accepted: already_within_standoff"):
            submit_approach(runtime,registry,object(),100,"r","p",1.5,.2,10)
