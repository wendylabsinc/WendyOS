"""ROS adapter. Only this node's executor touches Nav2 handles and ROS publishers."""
import json
import math
from pathlib import Path
import queue
import threading
import time

from .runtime import NavigationRuntime, RuntimeConfig, SensorRequirement
from .grounding import GroundingConfig, TargetRegistry, RobotPose


def stamp_seconds(stamp):
    return stamp.sec + stamp.nanosec / 1e9


def attitude(q):
    values = (q.x, q.y, q.z, q.w)
    if not all(math.isfinite(v) for v in values) or abs(sum(v*v for v in values) - 1) > .02:
        raise ValueError("orientation quaternion is invalid")
    x, y, z, w = values
    return (math.atan2(2*(w*x+y*z), 1-2*(x*x+y*y)),
            math.asin(max(-1, min(1, 2*(w*y-z*x)))),
            math.atan2(2*(w*z+x*y), 1-2*(y*y+z*z)))


class NavigationActions:
    """Action transport bookkeeping; pure Python and testable without ROS.

    All commands/callbacks run on the ROS executor thread. The lock only makes
    diagnostic snapshots safe for MCP readers. Transport failure is uncertainty,
    never proof that a remotely accepted navigation goal has finished.
    """
    def __init__(self, runtime, client, make_goal, feedback, clock=time.monotonic):
        self.runtime, self.client = runtime, client
        self.make_goal, self.feedback, self.clock = make_goal, feedback, clock
        self.handles, self.submissions = {}, set()
        self.pending_cancel, self.uncertain = set(), {}
        self.result_tokens, self.result_retry, self.cancel_retry = {}, {}, {}
        self.cancel_pending = {}
        self._lock = threading.RLock()

    def _uncertain(self, goal_id, reason):
        with self._lock:
            self.uncertain[goal_id] = reason
        self.runtime.backend_uncertain(goal_id, reason)

    def send(self, goal_id, pose, speed):
        with self._lock:
            if self.handles or self.submissions:
                self._uncertain(goal_id, "previous_action_not_settled")
                return
        if not self.client.server_is_ready():
            self.runtime.backend_result(goal_id, "rejected")
            return
        try:
            goal = self.make_goal(pose)
        except Exception:
            self.runtime.backend_result(goal_id, "rejected")
            return  # Nothing was submitted to the server.
        with self._lock:
            self.submissions.add(goal_id)
        try:
            future = self.client.send_goal_async(goal, feedback_callback=lambda msg: self.feedback(goal_id, msg))
            future.add_done_callback(lambda done: self.accepted(goal_id, done))
        except Exception:
            self._uncertain(goal_id, "goal_submission_transport_failed")

    def accepted(self, goal_id, future):
        try:
            handle = future.result()
            accepted = handle.accepted
        except Exception:
            self._uncertain(goal_id, "goal_acceptance_unknown")
            return  # Keep the pending submission as a barrier to another goal.
        with self._lock:
            self.submissions.discard(goal_id)
            if accepted:
                self.handles[goal_id] = handle
        if not accepted:
            self._forget(goal_id)
            self.runtime.backend_result(goal_id, "rejected")
            return
        current = self.runtime.status()["active_goal"]
        should_cancel = (goal_id in self.pending_cancel or not current
                         or current["goal_id"] != goal_id or current["state"] != "submitting")
        if should_cancel:
            self.cancel(goal_id)
        watching = self._watch_result(goal_id)
        if not should_cancel and watching:
            self.runtime.backend_accepted(goal_id)

    def _watch_result(self, goal_id):
        with self._lock:
            handle = self.handles.get(goal_id)
            if handle is None or goal_id in self.result_tokens:
                return False
            token = object()
            self.result_tokens[goal_id] = token
            self.result_retry.pop(goal_id, None)
        try:
            future = handle.get_result_async()
            future.add_done_callback(lambda done: self.result(goal_id, done, token))
            return True
        except Exception:
            with self._lock:
                self.result_tokens.pop(goal_id, None)
                self.result_retry[goal_id] = self.clock() + .5
            self._uncertain(goal_id, "goal_result_transport_failed")
            return False

    def result(self, goal_id, future, token):
        with self._lock:
            if self.result_tokens.get(goal_id) is not token:
                return  # A completed/retried result cannot resurrect old state.
            self.result_tokens.pop(goal_id, None)
        try:
            outcome = {4: "succeeded", 5: "cancelled", 6: "failed"}.get(future.result().status)
            if outcome is None:
                raise ValueError("nonterminal action result")
        except Exception:
            with self._lock:
                self.result_retry[goal_id] = self.clock() + .5
            self._uncertain(goal_id, "goal_terminal_state_unknown")
            return
        self._forget(goal_id)
        self.runtime.backend_result(goal_id, outcome)

    def cancel(self, goal_id):
        old_future = None
        with self._lock:
            handle = self.handles.get(goal_id)
            never_submitted = handle is None and goal_id not in self.submissions
            if not never_submitted:
                self.pending_cancel.add(goal_id)
                pending = self.cancel_pending.get(goal_id)
                if pending and self.clock() - pending[1] < .5:
                    return  # Only one outstanding cancellation request per goal.
                if pending:
                    old_future = self.cancel_pending.pop(goal_id)[0]
                self.cancel_retry[goal_id] = self.clock() + .5
        if old_future is not None:
            old_future.cancel()  # rclpy removes its pending request on completion.
        if never_submitted:
            # A queued send may have been skipped after cancellation, or this
            # goal already returned a terminal result. Do not create a ghost.
            self.runtime.backend_settled(goal_id)
            return
        if handle is None:
            return  # A late acceptance callback must cancel it before enabling.
        try:
            future = handle.cancel_goal_async()
            with self._lock:
                self.cancel_pending[goal_id] = (future, self.clock())
            future.add_done_callback(lambda done: self._cancel_response(goal_id, handle, done))
        except Exception:
            self._uncertain(goal_id, "goal_cancellation_transport_failed")

    def _cancel_response(self, goal_id, handle, future):
        with self._lock:
            pending = self.cancel_pending.get(goal_id)
            if (self.handles.get(goal_id) is not handle or pending is None
                    or pending[0] is not future):
                return
            self.cancel_pending.pop(goal_id, None)
        try:
            if future.result().return_code != 0:
                raise ValueError("cancellation not accepted")
        except Exception:
            self._uncertain(goal_id, "goal_cancellation_not_acknowledged")
        # Even successful cancellation acknowledgment is NOT a terminal result.

    def _forget(self, goal_id):
        with self._lock:
            self.handles.pop(goal_id, None)
            self.submissions.discard(goal_id)
            self.pending_cancel.discard(goal_id)
            self.uncertain.pop(goal_id, None)
            self.result_tokens.pop(goal_id, None)
            self.result_retry.pop(goal_id, None)
            self.cancel_retry.pop(goal_id, None)
            pending = self.cancel_pending.pop(goal_id, None)
        if pending:
            pending[0].cancel()

    def retry(self):
        now = self.clock()
        with self._lock:
            results = [goal_id for goal_id, at in self.result_retry.items() if now >= at]
            cancels = [goal_id for goal_id, at in self.cancel_retry.items()
                       if now >= at and goal_id in self.handles]
        for goal_id in results:
            self._watch_result(goal_id)
        for goal_id in cancels:
            self.cancel(goal_id)

    def allows_motion(self):
        current = self.runtime.status()["active_goal"]
        if not current or current["state"] != "running":
            return False
        goal_id = current["goal_id"]
        with self._lock:
            return (set(self.handles) == {goal_id} and not self.submissions
                    and not self.pending_cancel and not self.uncertain)

    def snapshot(self):
        with self._lock:
            return {"pending_submissions": sorted(self.submissions),
                    "active_handles": sorted(self.handles),
                    "pending_cancellation": sorted(self.pending_cancel),
                    "uncertain_goals": dict(self.uncertain)}


class Nav2LifecycleMonitor:
    """Async lifecycle readiness; stale/missing responses never imply active."""
    def __init__(self, clients, request_factory, clock=time.monotonic):
        self.clients, self.request_factory, self.clock = clients, request_factory, clock
        self.samples, self.pending, self.next_query = {}, {}, {}
        self._lock = threading.RLock()

    def poll(self):
        now = self.clock()
        for name, client in self.clients.items():
            with self._lock:
                pending = self.pending.get(name)
                if pending and now - pending[1] >= .3:
                    self.pending.pop(name, None)
                    self.samples[name] = (now, None)
                    try:
                        client.remove_pending_request(pending[0])
                        pending[0].cancel()
                    except Exception:
                        # A completed request can race this best-effort cancellation.
                        pass
                if name in self.pending or now < self.next_query.get(name, 0):
                    continue
                self.next_query[name] = now + .2
            if not client.service_is_ready():
                with self._lock:
                    self.samples[name] = (now, None)
                continue
            try:
                future = client.call_async(self.request_factory())
                with self._lock:
                    self.pending[name] = (future, now)
                future.add_done_callback(lambda done, name=name: self._response(name, done))
            except Exception:
                with self._lock:
                    self.samples[name] = (now, None)

    def _response(self, name, future):
        with self._lock:
            pending = self.pending.get(name)
            if pending is None or pending[0] is not future:
                return
            self.pending.pop(name, None)
            try:
                state = future.result().current_state.id
            except Exception:
                state = None
            self.samples[name] = (self.clock(), state)

    def snapshot(self):
        now = self.clock()
        with self._lock:
            return {name: {"state_id": self.samples.get(name, (None, None))[1],
                           "fresh": name in self.samples and 0 <= now-self.samples[name][0] <= .5}
                    for name in self.clients}

    def ready(self):
        states = self.snapshot()
        return bool(states) and all(s["fresh"] and s["state_id"] == 3 for s in states.values())


class QueuedBackend:
    def __init__(self):
        self.commands = queue.SimpleQueue()

    def send_goal(self, goal_id, pose, max_speed):
        self.commands.put(("send", (goal_id, pose, max_speed)))

    def cancel_goal(self, goal_id):
        self.commands.put(("cancel", (goal_id,)))

    def set_enabled(self, enabled):
        self.commands.put(("enable", (enabled,)))

    def stop(self):
        self.commands.put(("stop", (False,)))

    def latch_stop(self):
        self.commands.put(("stop", (True,)))


def submit_approach(runtime, registry, robot_pose, now, request_id, target_id,
                    standoff, max_speed, lease_seconds):
    metadata = {"kind": "approach_person", "target_id": target_id, "standoff": standoff,
                "max_speed": max_speed, "lease_seconds": lease_seconds}
    previous = runtime.request_status(request_id)
    if previous is not None:
        if previous.get("metadata") != metadata:
            raise ValueError("request_id was already used with different approach parameters")
        return {"goal": previous, "grounding": {"status": "reused_request", "target_id": target_id}}
    if robot_pose is None:
        raise ValueError("navigation pose is unavailable")
    choice = registry.choose_goal(target_id, robot_pose, now, standoff=standoff)
    if choice["goal"] is None:
        raise ValueError(f"no goal accepted: {choice.get('status', 'target is not approachable')}")
    pose = {key: choice["goal"][key] for key in ("frame_id", "x", "y", "yaw")}
    target = registry.status(target_id, now)
    if target["source_stamp"] != choice["target_source_stamp"]:
        raise ValueError("no goal accepted: target changed during grounding; inspect it again")
    runtime.update_target(target_id, target["source_stamp"], target["pose"]["x"],
                          target["pose"]["y"], target["frame_id"], valid=target["status"] == "observed")
    goal = runtime.navigate(request_id, pose, max_speed, lease_seconds=lease_seconds,
                            target_id=target_id, metadata=metadata,
                            expected_target_stamp=choice["target_source_stamp"])
    return {"goal": goal, "grounding": choice}


def sync_targets(runtime, registry, now, navigation_frame):
    for target in registry.observed_targets(now):
        pose = target.get("pose", {})
        runtime.update_target(target["target_id"], target["source_stamp"],
            pose.get("x", 0), pose.get("y", 0), target["frame_id"],
            valid=target.get("status") == "observed")
    active = runtime.status()["active_goal"]
    reference = active.get("target") if active else None
    if reference and registry.status(reference["target_id"], now)["status"] != "observed":
        # Explicit loss/retirement must stop now, even if the last observation
        # remains within its age budget. Unknown IDs cannot inherit old validity.
        runtime.update_target(reference["target_id"], now, reference["x"],
                              reference["y"], navigation_frame, valid=False)


def create_node(settings):
    import rclpy
    from rclpy.node import Node
    from rclpy.action import ActionClient
    from rclpy.qos import qos_profile_sensor_data
    from rclpy.time import Time
    from nav2_msgs.action import NavigateToPose
    from nav2_msgs.msg import SpeedLimit
    from lifecycle_msgs.srv import GetState
    from nav_msgs.msg import Odometry
    from sensor_msgs.msg import Imu
    from std_msgs.msg import String
    from tf2_ros import Buffer, TransformListener, TransformException
    from .perception import Perception

    class NavigationNode(Node):
        def __init__(self):
            super().__init__("wendy_navigation_supervisor")
            self.settings = settings
            raw = settings["runtime"]
            self.cfg = RuntimeConfig(**raw, sensors=(
                SensorRequirement("scan", raw["base_frame"], .3, .3),
                SensorRequirement("imu", raw["base_frame"], .3, .3),
                SensorRequirement("transforms", raw["navigation_frame"], .3, .3),
                SensorRequirement("motor", raw["base_frame"], .3, .3),
            ), allowed_postures=("upright",))
            self.backend = QueuedBackend()
            Path(settings["database"]).parent.mkdir(parents=True, exist_ok=True)
            self.runtime = NavigationRuntime(self.cfg, settings["database"], self.backend,
                                             source_clock=self.source_now)
            self.tf = Buffer()
            self.listener = TransformListener(self.tf, self)
            self.nav = ActionClient(self, NavigateToPose, "/robot_navigation/navigate_to_pose")
            self.lifecycle = Nav2LifecycleMonitor({
                name: self.create_client(GetState, f"/robot_navigation/{name}/get_state")
                for name in ("planner_server", "controller_server", "bt_navigator")
            }, GetState.Request)
            self.permit_pub = self.create_publisher(String, "/robot_navigation/permit", 1)
            self.stop_pub = self.create_publisher(String, "/robot_navigation/stop", 1)
            self.speed_pub = self.create_publisher(SpeedLimit, "/robot_navigation/speed_limit", 1)
            self.enabled = False
            self.speed = self.cfg.max_linear_speed
            self.actions = NavigationActions(self.runtime, self.nav, self._make_goal, self._feedback)
            self.feedback = {}
            self.guard_status = self.motor_status = None
            self.imu = None
            self.imu_received = None
            self.robot_pose = None
            self._snapshot_lock = threading.RLock()
            self._approach_lock = threading.RLock()
            self.registry = TargetRegistry(GroundingConfig(
                nav_frame=self.cfg.navigation_frame,
                robot_radius=settings["guard"]["footprint_radius"],
                goal_tolerance=.15, target_drift_margin=self.cfg.target_max_displacement))
            self.perception = Perception(self, settings, self.registry)
            self.create_subscription(Odometry, settings["topics"]["odometry"], self.on_odom, qos_profile_sensor_data)
            self.create_subscription(Imu, settings["topics"]["imu"], self.on_imu, qos_profile_sensor_data)
            self.create_subscription(String, "/robot_navigation/guard_status", self.on_guard, 1)
            self.create_subscription(String, "/robot_navigation/motor_status", self.on_motor, 1)
            self.create_timer(.05, self.tick)

        def source_now(self):
            return self.get_clock().now().nanoseconds / 1e9

        def on_imu(self, msg):
            try:
                roll, pitch, _ = attitude(msg.orientation)
                healthy = (msg.header.frame_id == self.cfg.base_frame
                           and msg.orientation_covariance[0] >= 0
                           and abs(roll) < .45 and abs(pitch) < .45)
                self.imu = (stamp_seconds(msg.header.stamp), healthy)
                self.imu_received = time.monotonic()
            except (ValueError, IndexError):
                self.imu = (stamp_seconds(msg.header.stamp), False)
            self.runtime.update_sensor("imu", stamp_seconds(msg.header.stamp), msg.header.frame_id,
                                       healthy=self.imu[1])

        def on_odom(self, msg):
            try:
                _, _, yaw = attitude(msg.pose.pose.orientation)
                cov = msg.pose.covariance
                position_variance, yaw_variance = max(cov[0], cov[7]), cov[35]
                # A zero-filled covariance has no measured uncertainty information.
                if cov[0] <= 0 or cov[7] <= 0 or yaw_variance <= 0:
                    position_variance = float("nan")
                imu_ok = (self.imu is not None and self.imu[1] and
                          0 <= self.source_now()-self.imu[0] <= .3 and
                          self.imu_received is not None and time.monotonic()-self.imu_received <= .3)
                self.runtime.update_odometry(
                    stamp_seconds(msg.header.stamp), msg.header.frame_id, msg.child_frame_id,
                    msg.pose.pose.position.x, msg.pose.pose.position.y, yaw,
                    math.hypot(msg.twist.twist.linear.x, msg.twist.twist.linear.y),
                    msg.twist.twist.angular.z, position_variance, yaw_variance,
                    "upright" if imu_ok else "unknown")
            except (ValueError, IndexError):
                self.runtime.update_odometry(stamp_seconds(msg.header.stamp), msg.header.frame_id,
                    msg.child_frame_id, 0, 0, 0, float("nan"), 0, 0, 0, "unknown")

        def on_guard(self, msg):
            try:
                data = json.loads(msg.data)
                stamp = data["source_stamp"]
                fresh = isinstance(stamp, (int, float)) and not isinstance(stamp, bool) and 0 <= self.source_now()-stamp <= .3
                self.runtime.update_guard(fresh and data.get("ready") is True,
                                          data.get("stop_latched", True))
                self.runtime.update_sensor("scan", data.get("scan_stamp", stamp),
                    data.get("frame_id", ""), healthy=fresh and data.get("sensors_ready") is True,
                    coverage_ok=data.get("coverage_ok") is True)
                with self._snapshot_lock:
                    self.guard_status = data
            except (ValueError, TypeError, KeyError):
                self.runtime.update_guard(False)

        def on_motor(self, msg):
            try:
                data = json.loads(msg.data)
                self.runtime.update_sensor("motor", data["source_stamp"], self.cfg.base_frame,
                                           healthy=data.get("ready") is True)
                with self._snapshot_lock:
                    self.motor_status = data
            except (ValueError, TypeError, KeyError):
                self.runtime.update_sensor("motor", self.source_now(), self.cfg.base_frame, healthy=False)

        def _make_goal(self, pose):
            goal = NavigateToPose.Goal()
            goal.pose.header.frame_id = pose["frame_id"]
            goal.pose.header.stamp = self.get_clock().now().to_msg()
            goal.pose.pose.position.x, goal.pose.pose.position.y = pose["x"], pose["y"]
            goal.pose.pose.orientation.z = math.sin(pose["yaw"]/2)
            goal.pose.pose.orientation.w = math.cos(pose["yaw"]/2)
            return goal

        def _feedback(self, goal_id, msg):
            f = msg.feedback
            with self._snapshot_lock:
                self.feedback = {"goal_id": goal_id, "distance_remaining": f.distance_remaining,
                                 "number_of_recoveries": f.number_of_recoveries,
                                 "source_stamp": stamp_seconds(f.current_pose.header.stamp)}

        def _drain(self):
            for _ in range(256):
                try:
                    operation, args = self.backend.commands.get_nowait()
                except queue.Empty:
                    return
                if operation == "enable":
                    self.enabled = args[0] and self.actions.allows_motion()
                elif operation == "send":
                    current = self.runtime.status()["active_goal"]
                    if current and current["goal_id"] == args[0] and current["state"] == "submitting":
                        self.speed = args[2]
                        self.actions.send(*args)
                elif operation == "cancel":
                    self.actions.cancel(args[0])
                elif operation == "stop":
                    self.enabled = False
                    self.stop_pub.publish(String(data=json.dumps({"latch": args[0]})))

        def tick(self):
            try:
                transform = self.tf.lookup_transform(self.cfg.navigation_frame, self.cfg.base_frame, Time())
                stamp = stamp_seconds(transform.header.stamp)
                p, q = transform.transform.translation, transform.transform.rotation
                _, _, yaw = attitude(q)
                self.runtime.update_navigation_pose(stamp, self.cfg.navigation_frame, p.x, p.y, yaw)
                self.runtime.update_sensor("transforms", stamp, self.cfg.navigation_frame)
                with self._snapshot_lock:
                    self.robot_pose = RobotPose(p.x, p.y, yaw, self.cfg.navigation_frame, stamp,
                                                uncertainty=math.sqrt(self.cfg.max_position_variance))
            except (TransformException, ValueError):
                self.runtime.update_sensor("transforms", self.source_now(), self.cfg.navigation_frame, healthy=False)
            self.lifecycle.poll()
            self.runtime.update_navigation_available(self.nav.server_is_ready() and self.lifecycle.ready())
            sync_targets(self.runtime, self.registry, self.source_now(), self.cfg.navigation_frame)
            self.actions.retry()
            self.runtime.tick()
            self._drain()
            self.permit_pub.publish(String(data=json.dumps({"enabled": self.enabled and self.actions.allows_motion(), "max_speed": self.speed})))
            limit = SpeedLimit()
            limit.header.stamp = self.get_clock().now().to_msg()
            limit.percentage = False
            limit.speed_limit = self.speed
            self.speed_pub.publish(limit)

        def robot_status(self):
            with self._snapshot_lock:
                return {**self.runtime.status(), "diagnostics": self.runtime.diagnostics(),
                        "guard": self.guard_status, "motor": self.motor_status,
                        "navigation_feedback": self.feedback, "navigation_actions": self.actions.snapshot(),
                        "navigation_lifecycle": self.lifecycle.snapshot(),
                        "targets": self.perception.robot_targets(),
                        "posture_source": "IMU tilt; upright does not establish gait or standing state"}

        def approach(self, request_id, target_id, standoff, max_speed, lease_seconds):
            with self._approach_lock:
                with self._snapshot_lock:
                    pose = self.robot_pose
                return submit_approach(self.runtime, self.registry, pose, self.source_now(),
                                       request_id, target_id, standoff, max_speed, lease_seconds)

    return NavigationNode()
