"""Local collaborative-office physics, simulated links and ray-derived mapping.

This adapter has no hardware transport. The Go coordinator exchanges JSON with
this process; individual robot links gate telemetry, commands and map delivery.
The viewer sees physical ground truth separately from the coordinator's cache.
"""
from __future__ import annotations

from collections import OrderedDict, deque
import copy
import hashlib
import heapq
import itertools
import json
import math
from pathlib import Path
import queue
import select
import subprocess
import threading
import time


MAX_SPEED = .55
COMMAND_TTL = .5
VOXEL_SIZE = .25


def finite_vector(value, length=2):
    return (isinstance(value, (list, tuple)) and len(value) == length
            and all(type(x) in (int, float) and math.isfinite(x) for x in value))


def follow_path(position, path, cursor=0):
    """Follow an already accepted route; never invent a route while offline."""
    while cursor < len(path) and math.dist(position, path[cursor]) < .075:
        cursor += 1
    if cursor == len(path):
        return [0., 0.], cursor
    delta = [path[cursor][i] - position[i] for i in range(2)]
    distance = math.hypot(*delta)
    speed = min(MAX_SPEED, distance * 3)
    return [x / distance * speed for x in delta], cursor


class VoxelMap:
    """Bounded occupied-hit history. Absence does not mean free space."""
    def __init__(self, limit=12000):
        if type(limit) is not int or limit < 1:
            raise ValueError("voxel limit must be positive")
        self.limit = limit
        self.cells = OrderedDict()
        self._oldest = []

    def observe(self, point, source, seconds):
        if (not finite_vector(point, 3) or not math.isfinite(seconds) or seconds < 0
                or not isinstance(source, str) or not source or len(source) > 100):
            raise ValueError("invalid LiDAR observation")
        key = tuple(math.floor(v / VOXEL_SIZE) for v in point)
        old = self.cells.get(key)
        if old is not None and (old["last_seen_s"], old["source"]) >= (seconds, source):
            return False
        self.cells[key] = {"position": [(v + .5) * VOXEL_SIZE for v in key],
                           "source": source, "last_seen_s": seconds}
        self.cells.move_to_end(key)
        heapq.heappush(self._oldest, (seconds, source, key))
        while len(self.cells) > self.limit:
            stamp, origin, victim = heapq.heappop(self._oldest)
            current = self.cells.get(victim)
            if current and (current["last_seen_s"], current["source"]) == (stamp, origin):
                del self.cells[victim]
        if len(self._oldest) > max(128, self.limit * 3):
            self._oldest = [(v["last_seen_s"], v["source"], k) for k, v in self.cells.items()]
            heapq.heapify(self._oldest)
        return key in self.cells and (self.cells[key]["last_seen_s"], self.cells[key]["source"]) == (seconds, source)

    def merge(self, other):
        changed = 0
        for item in other.cells.values():
            changed += self.observe(item["position"], item["source"], item["last_seen_s"])
        return changed

    def snapshot(self):
        return [dict(v, position=list(v["position"])) for _, v in sorted(self.cells.items())]


class SimulatedMesh:
    """Undirected links with routed messages and one-hop map gossip rounds.

    A round snapshots all peers before delivery, so data crosses one link per
    round. Packet counts represent modeled application messages, not OS traffic.
    """
    def __init__(self, robots, positions=None, max_distance_m=6.):
        self.nodes = sorted(["hub", *robots])
        if len(set(self.nodes)) != len(self.nodes):
            raise ValueError("mesh node identifiers must be unique")
        self.edges = {}
        self.maps = {node: VoxelMap() for node in self.nodes}
        if not math.isfinite(max_distance_m) or max_distance_m <= 0:
            raise ValueError("radio range must be positive and finite")
        self.max_distance_m = float(max_distance_m)
        self.positions = None
        self.internet = {"id": "hub--internet", "a": "hub", "b": "internet",
                         "enabled": False, "up": False, "reason": "disabled",
                         "packets": 0, "external_traffic": False}
        for a, b in itertools.combinations(self.nodes, 2):
            edge_id = self.edge_id(a, b)
            self.edges[edge_id] = {"id": edge_id, "a": a, "b": b, "enabled": True,
                "up": True, "distance_m": None, "in_range": True, "reason": "connected",
                "packets": 0, "telemetry_packets": 0, "command_packets": 0,
                "gossip_packets": 0}
        self.rounds = 0
        if positions is not None:
            self.set_positions(positions)

    @staticmethod
    def edge_id(a, b):
        return "--".join(sorted((a, b)))

    def set_positions(self, positions):
        if set(positions) != set(self.nodes) or any(not finite_vector(p) for p in positions.values()):
            raise ValueError("radio positions must contain every local node's finite XY position")
        self.positions = {node: list(position) for node, position in positions.items()}
        self._update_edges()

    def _update_edges(self):
        for edge in self.edges.values():
            distance = None if self.positions is None else math.dist(self.positions[edge["a"]], self.positions[edge["b"]])
            edge["distance_m"] = distance
            edge["in_range"] = distance is None or distance <= self.max_distance_m
            edge["up"] = edge["enabled"] and edge["in_range"]
            edge["reason"] = "disabled" if not edge["enabled"] else "connected" if edge["in_range"] else "out_of_range"

    def route(self, source, target="hub"):
        if source not in self.maps or target not in self.maps:
            raise ValueError("unknown mesh endpoint")
        waiting = deque([[source]])
        seen = {source}
        while waiting:
            path = waiting.popleft()
            if path[-1] == target:
                return path
            for peer in self.nodes:
                edge = self.edges.get(self.edge_id(path[-1], peer))
                if peer not in seen and edge and edge["up"]:
                    seen.add(peer)
                    waiting.append(path + [peer])
        return []

    def toggle(self, edge_id):
        if edge_id == self.internet["id"]:
            enabled = not self.internet["enabled"]
            self.internet.update(enabled=enabled, up=enabled, reason="connected" if enabled else "disabled")
            return enabled
        edge = self.edges[edge_id]
        edge["enabled"] = not edge["enabled"]
        self._update_edges()
        return edge["enabled"]

    def deliver(self, source, target, kind):
        if kind not in ("telemetry", "command"):
            raise ValueError("unknown routed message kind")
        route = self.route(source, target)
        for a, b in zip(route, route[1:]):
            edge = self.edges[self.edge_id(a, b)]
            edge["packets"] += 1
            edge[kind + "_packets"] += 1
        return route

    def gossip(self):
        snapshots = {node: self.maps[node].snapshot() for node in self.nodes}
        updates = {node: {} for node in self.nodes}
        for edge in self.edges.values():
            if not edge["up"]:
                continue
            for source, target in ((edge["a"], edge["b"]), (edge["b"], edge["a"])):
                records = snapshots[source]
                if not records:
                    continue
                edge["packets"] += 1
                edge["gossip_packets"] += 1
                for item in records:
                    if self.maps[target].observe(item["position"], item["source"], item["last_seen_s"]):
                        origin = item["source"]
                        updates[target][origin] = updates[target].get(origin, 0) + 1
        self.rounds += 1
        return updates

    def snapshot(self):
        return {"mode": "simulated peer-to-peer mesh", "nodes": list(self.nodes),
            "edges": [dict(e) for e in self.edges.values()],
            "routes": {n: self.route(n) for n in self.nodes if n != "hub"},
            "peer_voxels": {n: len(m.cells) for n, m in self.maps.items()},
            "gossip_rounds": self.rounds,
            "internet": dict(self.internet), "max_link_distance_m": self.max_distance_m,
            "range_model": "planar Euclidean distance cutoff" if self.positions is not None else "abstract test graph",
            "semantics": "Local routed messages and one-hop map gossip; separate simulated internet uplink; no external network traffic"}


class RobotLink:
    """An application-message gate, not a claim of an OS network partition."""
    def __init__(self, config):
        self.config = config
        self.online = True
        self.available = True
        self.delivered = {"id": config["id"], "position": list(config["start"]),
                          "velocity": [0., 0.], "online": True, "available": True}
        self.command = [0., 0.]
        self.path = []
        self.cursor = 0
        self.state = "idle"
        self.job_id = ""
        self.last_command_wall = -math.inf
        self.minimum_response_sequence = 0
        self.local_map = VoxelMap(4000)
        self.lidar_points = []
        self.lidar_rays = []
        self.stats = {"telemetry_attempted": 0, "telemetry_delivered": 0,
                      "telemetry_dropped": 0, "commands_delivered": 0,
                      "commands_dropped": 0, "map_frames_delivered": 0,
                      "map_frames_buffered": 0, "voxels_merged": 0}

    def telemetry(self, position, velocity):
        self.stats["telemetry_attempted"] += 1
        if self.online:
            self.delivered = {"id": self.config["id"], "position": list(position),
                              "velocity": list(velocity), "available": self.available,
                              "online": True}
            self.stats["telemetry_delivered"] += 1
        else:
            self.stats["telemetry_dropped"] += 1
        # Even availability changes are unknown to the coordinator while cut.
        return {**copy.deepcopy(self.delivered), "online": self.online}

    def accept(self, command, wall, sequence=None):
        if not self.online or (sequence is not None and sequence < self.minimum_response_sequence):
            self.stats["commands_dropped"] += 1
            return False
        velocity, path = command.get("velocity"), command.get("path", [])
        if (not finite_vector(velocity) or math.hypot(*velocity) > MAX_SPEED + 1e-6
                or not isinstance(path, list) or len(path) > 4096
                or any(not finite_vector(p) for p in path)):
            raise ValueError("invalid coordinator command")
        self.command = list(velocity)
        self.path = copy.deepcopy(path)
        self.cursor = 0
        self.state = str(command.get("state", "unknown"))[:100]
        self.job_id = str(command.get("job_id", ""))[:100]
        self.last_command_wall = wall
        self.stats["commands_delivered"] += 1
        return True

    def velocity(self, position, wall, active, coordinator_healthy):
        if not active or not coordinator_healthy or not self.available:
            return [0., 0.]
        # Track cached progress while connected too, so cutting a link does not
        # send the robot back to an already traversed route prefix.
        cached, self.cursor = follow_path(position, self.path, self.cursor)
        if self.online:
            return list(self.command) if wall - self.last_command_wall <= COMMAND_TTL else [0., 0.]
        if not self.job_id or not self.path:
            return [0., 0.]
        self.state = "awaiting_reconnect" if self.cursor == len(self.path) else "cached_route"
        return cached

    def publish_map(self, shared):
        if self.online:
            self.stats["voxels_merged"] += shared.merge(self.local_map)
            self.stats["map_frames_delivered"] += 1
        else:
            self.stats["map_frames_buffered"] += 1


class CoordinatorPipe:
    """Single in-flight exchange. Native rendering never waits for subprocess IO."""
    def __init__(self, binary, scene):
        self.process = subprocess.Popen([str(Path(binary).resolve()), "--scene", str(scene)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True, bufsize=1)
        self.requests = queue.Queue(maxsize=1)
        self.responses = queue.Queue(maxsize=2)
        self.done = threading.Event()
        self.error = None
        self.thread = threading.Thread(target=self._run, name="collab-coordinator", daemon=True)
        self.thread.start()

    def submit(self, request):
        if self.error or self.done.is_set():
            return False
        try:
            self.requests.put_nowait(request)
            return True
        except queue.Full:
            return False

    def poll(self):
        result = []
        while True:
            try:
                result.append(self.responses.get_nowait())
            except queue.Empty:
                return result

    def _run(self):
        try:
            while not self.done.is_set():
                try:
                    request = self.requests.get(timeout=.1)
                except queue.Empty:
                    continue
                self.process.stdin.write(json.dumps(request, allow_nan=False, separators=(",", ":")) + "\n")
                self.process.stdin.flush()
                if not select.select([self.process.stdout], [], [], COMMAND_TTL)[0]:
                    raise TimeoutError("coordinator response exceeded 500 ms")
                line = self.process.stdout.readline((1 << 20) + 1)
                if not line.endswith("\n") or len(line) > 1 << 20:
                    raise ValueError("coordinator ended or response exceeded bound")
                response = json.loads(line)
                if (response.get("run_id") != request["run_id"]
                        or response.get("sequence") != request["sequence"]
                        or response.get("time_s") != request["time_s"]):
                    raise ValueError("coordinator response identity or clock mismatch")
                self.responses.put((response, time.monotonic()), timeout=COMMAND_TTL)
        except Exception as exc:
            if not self.done.is_set():
                self.error = f"{type(exc).__name__}: {exc}"

    def close(self):
        self.done.set()
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=2)
        self.thread.join(timeout=1)
        for stream in (self.process.stdin, self.process.stdout):
            try:
                stream.close()
            except BrokenPipeError:
                pass  # A crashed coordinator may leave buffered stdin bytes.


class SceneRuntime:
    """Simulation-only adapter. Call tick from the thread owning MjData."""
    def __init__(self, scene_path, coordinator_binary, run_id):
        import mujoco
        import numpy as np

        if mujoco.__version__ != "4.0.0":
            raise ValueError("collaborative scene requires MuJoCo 4.0.0")
        if not isinstance(run_id, str) or not run_id or len(run_id) > 100:
            raise ValueError("run_id must be a nonempty bounded string")
        self.mj, self.np = mujoco, np
        self.scene_path = Path(scene_path).resolve()
        self.config = json.loads(self.scene_path.read_text())
        if self.config.get("schema_version") != "wendy.collaborative-office.scene.v1":
            raise ValueError("unsupported collaborative scene")
        model_path = (self.scene_path.parent / self.config["model"]["path"]).resolve()
        if not model_path.is_relative_to(self.scene_path.parent):
            raise ValueError("scene model must remain inside its package")
        xml = model_path.read_bytes()
        if hashlib.sha256(xml).hexdigest() != self.config["model"]["sha256"]:
            raise ValueError("scene model digest mismatch")
        # This procedural scene has no external assets. Compile verified bytes.
        self.model = mujoco.MjModel.from_xml_string(xml.decode())
        self.data = mujoco.MjData(self.model)
        self.run_id = run_id
        self.sequence = 0
        self._accepted_sequence = 0
        self.authorized = False
        self.authorization_started_s = None
        self.stopped = False
        self.fault = None
        self.phase = "awaiting_authorization"
        self.jobs = copy.deepcopy(self.config["jobs"])
        self.exclusions = []
        self.route_preview = None
        self.preview_request = {"robot_id":self.config["robots"][0]["id"], "job_id":self.config["jobs"][0]["id"]}
        self.route_config_changed = False
        self.exclusions_confirmed = []
        self.completed = 0
        self.events = deque(maxlen=24)
        self.links = {r["id"]: RobotLink(r) for r in self.config["robots"]}
        if len(self.links) != len(self.config["robots"]) or len(self.links) != 3:
            raise ValueError("scene must contain three uniquely identified robots")
        self.bindings = {}
        for robot in self.config["robots"]:
            body = mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_BODY, robot["body"])
            joints = [mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_JOINT, n) for n in robot["joints"]]
            actuators = [mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_ACTUATOR, n) for n in robot["actuators"]]
            if body <= 0 or len(joints) != 2 or len(actuators) != 2 or min(*joints, *actuators) < 0:
                raise ValueError("scene robot binding is invalid")
            self.bindings[robot["id"]] = {"body": body, "dofs": [int(self.model.jnt_dofadr[j]) for j in joints],
                                          "actuators": actuators}
        if self.model.nv != 6 or self.model.nu != 6 or not 0 < self.model.opt.timestep <= .02:
            raise ValueError("expected three planar two-axis robots and bounded timestep")
        self.mesh = SimulatedMesh(list(self.links))
        self._routes = {robot_id: self.mesh.route(robot_id) for robot_id in self.links}
        self.shared_map = self.mesh.maps["hub"]
        self.last_scan_s = -math.inf
        self.last_request_s = -math.inf
        self.last_response_wall = -math.inf
        self.last_tick_wall = time.monotonic()
        self.elapsed_credit = 0.
        self.last_request = None
        self.closed = False
        mujoco.mj_forward(self.model, self.data)
        self._update_radio_positions()
        self._refresh_routes()
        self._event("Simulated internet uplink starts disabled; local radio mesh remains available; no external traffic")
        self.transport = CoordinatorPipe(coordinator_binary, self.scene_path)

    def _event(self, text):
        self.events.append({"time_s": round(float(self.data.time), 3), "text": text})

    def set_exclusions(self, regions):
        """Edit only pre-start operator configuration; never teleport or move geometry."""
        if self.authorization_started_s is not None or self.stopped or self.fault:
            self._event("Route exclusions are frozen after Start; begin a new simulation to edit them")
            return False
        try:
            grid = self.config['grid']
            if not isinstance(regions, list) or len(regions) > 16:
                raise ValueError('Use at most 16 rectangular exclusions')
            ids = set()
            for r in regions:
                if (not isinstance(r, dict) or set(r) != {'id','min','max'} or
                        not isinstance(r['id'],str) or not 1 <= len(r['id'].encode()) <= 64 or r['id'] in ids or
                        not finite_vector(r['min']) or not finite_vector(r['max'])):
                    raise ValueError('Invalid exclusion rectangle')
                ids.add(r['id'])
                for axis, dimension in ((0,'width'),(1,'height')):
                    lo,hi = grid['origin'][axis],grid['origin'][axis]+grid[dimension]*grid['resolution']
                    if not lo <= r['min'][axis] < r['max'][axis] <= hi:
                        raise ValueError('Choose two different floor points inside the known office grid')
                # Same closed-cell conservative rasterization as the Go planner.
                for rid in self.links:
                    position,_ = self._pose(rid)
                    low = [grid['origin'][a]+math.floor((position[a]-grid['origin'][a])/grid['resolution'])*grid['resolution'] for a in (0,1)]
                    if all(low[a] <= r['max'][a]+.32 and low[a]+grid['resolution'] >= r['min'][a]-.32 for a in (0,1)):
                        raise ValueError('Exclusion touches a robot clearance; choose a different rectangle')
        except (ValueError,KeyError,TypeError) as exc:
            self._event(str(exc));return False
        self.exclusions = copy.deepcopy(regions)
        self.route_preview = None
        self.route_config_changed = True
        self.last_request_s = -math.inf
        self._event(f"Reviewing {len(regions)} operator route exclusions; no geometry or sensing changed")
        return True

    def select_preview(self, robot_id, job_id):
        if self.authorization_started_s is not None:
            return False
        if robot_id not in self.links or job_id not in {j['id'] for j in self.config['jobs']}:
            return False
        self.preview_request = {'robot_id':robot_id,'job_id':job_id}
        self.route_preview = None
        self.last_request_s = -math.inf
        return True

    def start_mission(self):
        if self.stopped or self.fault or self._expired():
            self._event("Start refused: restart the demo after stop, expiry or a coordinator fault")
            return False
        if getattr(self,'route_config_changed',False) and (self.exclusions_confirmed != self.exclusions or self.route_preview is None):
            self._event("Start waits for the current route-exclusion preview; review it before starting")
            return False
        if self.authorization_started_s is None:
            self.authorization_started_s = float(self.data.time)
        self.authorized = True
        self._event("Operator authorized the synthetic team inspection")
        self.last_request_s = -math.inf
        return True

    def _expired(self):
        return (self.authorization_started_s is not None
                and float(self.data.time) >= self.authorization_started_s + 600)

    def stop_mission(self):
        self.stopped = True
        self.authorized = False
        self.phase = "canceled"
        self._brake_all()
        self._event("Operator stopped all simulated robots locally; restart to authorize again")
        self.last_request_s = -math.inf

    def toggle_link(self, robot_id):
        if robot_id not in self.links:
            raise ValueError("unknown robot")
        return self.toggle_edge(self.mesh.edge_id(robot_id, "hub"))

    def _refresh_routes(self):
        for robot_id, link in self.links.items():
            route = self.mesh.route(robot_id)
            online = bool(route)
            if online != link.online:
                # A reconnection must receive a command computed from a new
                # telemetry request. An old in-flight reply cannot revive it.
                link.last_command_wall = -math.inf
                link.minimum_response_sequence = self.sequence + 1
            link.online = online
            self._routes[robot_id] = route

    def _update_radio_positions(self):
        positions = {"hub": [0., 0.]}
        positions.update({robot_id: self._pose(robot_id)[0][:2] for robot_id in self.links})
        self.mesh.set_positions(positions)

    def toggle_edge(self, edge_id):
        before = {n: list(route) for n, route in self._routes.items()}
        enabled = self.mesh.toggle(edge_id)
        if edge_id == self.mesh.internet["id"]:
            self._event(f"Simulated internet uplink {'enabled' if enabled else 'disabled'}; local routes unchanged; no external traffic")
            return enabled
        self._refresh_routes()
        changes = []
        for robot_id, route in self._routes.items():
            if route != before[robot_id]:
                changes.append(robot_id + (" via " + " > ".join(route[1:]) if route else " isolated from hub"))
        edge = self.mesh.edges[edge_id]
        self._event(f"{edge_id}: {edge['reason']}" + ("; " + "; ".join(changes) if changes else "; current hub routes unchanged"))
        self.last_request_s = -math.inf
        return enabled

    def toggle_available(self, robot_id):
        link = self.links[robot_id]
        link.available = not link.available
        link.last_command_wall = -math.inf
        link.minimum_response_sequence = self.sequence + 1
        if not link.available:
            self._brake(robot_id)
        self._event(f"{robot_id}: {'available' if link.available else 'paused locally'}")
        self.last_request_s = -math.inf
        return link.available

    def _brake(self, robot_id):
        binding = self.bindings[robot_id]
        self.data.ctrl[binding["actuators"]] = 0
        self.data.qvel[binding["dofs"]] = 0

    def _brake_all(self):
        self.data.ctrl[:] = 0
        self.data.qvel[:] = 0
        self.mj.mj_forward(self.model, self.data)

    def _pose(self, robot_id):
        binding = self.bindings[robot_id]
        return ([float(x) for x in self.data.xpos[binding["body"]]],
                [float(self.data.qvel[d]) for d in binding["dofs"]])

    def _request(self):
        if self.transport.requests.full():
            return  # Do not label telemetry delivered when the pipe is backlogged.
        self.sequence += 1
        robots = []
        for robot_id, link in self.links.items():
            position, velocity = self._pose(robot_id)
            robots.append(link.telemetry(position[:2], velocity))
        request = {"run_id": self.run_id, "sequence": self.sequence,
                   "time_s": float(self.data.time), "authorized": self.authorized,
                   "stop": self.stopped, "robots": robots,
                   "exclusions": copy.deepcopy(getattr(self,"exclusions",[])),
                   "preview": copy.deepcopy(getattr(self,"preview_request",None))}
        if self.transport.submit(request):
            self.last_request = request
            self.last_request_s = float(self.data.time)
            for robot_id, link in self.links.items():
                link.last_telemetry_route = self.mesh.deliver(robot_id, "hub", "telemetry")

    def _responses(self):
        for response, wall in self.transport.poll():
            if response["sequence"] <= self._accepted_sequence:
                raise ValueError("coordinator response regressed")
            commands = response.get("robots")
            if (not isinstance(commands, list) or len(commands) != len(self.links)
                    or {r.get("id") for r in commands} != set(self.links)):
                raise ValueError("coordinator robot set mismatch")
            # Validate all commands before applying any one of them.
            for command in commands:
                if (not finite_vector(command.get("velocity"))
                        or math.hypot(*command["velocity"]) > MAX_SPEED + 1e-6
                        or not isinstance(command.get("path", []), list)
                        or len(command.get("path", [])) > 4096
                        or any(not finite_vector(p) for p in command.get("path", []))):
                    raise ValueError("coordinator returned malformed motion")
            received_exclusions = response.get("exclusions",[])
            preview = response.get("preview")
            if received_exclusions == getattr(self,"exclusions",[]) and preview and all(preview.get(k)==v for k,v in self.preview_request.items()):
                self.route_preview = copy.deepcopy(preview)
                self.exclusions_confirmed = copy.deepcopy(received_exclusions)
            self._accepted_sequence = response["sequence"]
            self.last_response_wall = wall
            if not self.stopped:
                self.phase = str(response.get("phase", "unknown"))
            if self.phase in ("refused", "expired", "failed"):
                self.fault = f"coordinator {self.phase}"
            for command in commands:
                link = self.links[command["id"]]
                if link.accept(command, wall, response["sequence"]):
                    link.last_command_route = self.mesh.deliver("hub", command["id"], "command")
            positions = {j["id"]: j["position"] for j in self.config["jobs"]}
            self.jobs = [{**copy.deepcopy(job), "position": positions[job["id"]]}
                         for job in response.get("jobs", self.jobs)]
            self.completed = int(response.get("completed", 0))
            for event in (response.get("events") or [])[-20:]:
                self._event(str(event)[:300])

    def _lidar(self):
        now = float(self.data.time)
        for robot_id, link in self.links.items():
            position, _ = self._pose(robot_id)
            points, rays = [], []
            for height in (.25, .65, 1.15):
                origin = self.np.array([position[0], position[1], height], dtype=float)
                for i in range(48):
                    angle = 2 * math.pi * i / 48
                    direction = self.np.array([math.cos(angle), math.sin(angle), 0.], dtype=float)
                    geom = self.np.zeros(1, dtype=self.np.int32)
                    distance = self.mj.mj_ray(self.model, self.data, origin, direction, None,
                                               True, self.bindings[robot_id]["body"], geom)
                    if .04 < distance <= 6.:
                        hit = (origin + distance * direction).tolist()
                        points.append(hit)
                        rays.append([origin.tolist(), hit])
                        link.local_map.observe(hit, robot_id, now)
            link.lidar_points, link.lidar_rays = points, rays
            self.mesh.maps[robot_id].merge(link.local_map)
            if not link.online:
                link.stats["map_frames_buffered"] += 1
        updates = self.mesh.gossip()
        for source, count in updates["hub"].items():
            if source in self.links:
                self.links[source].stats["map_frames_delivered"] += 1
                self.links[source].stats["voxels_merged"] += count
        self.last_scan_s = now

    def tick(self, dt=.02):
        if self.closed:
            return
        if type(dt) not in (int, float) or not math.isfinite(dt) or not 0 < dt <= .05:
            raise ValueError("tick must be 0 < dt <= 0.05; never catch up stalled wall time")
        wall = time.monotonic()
        self._update_radio_positions()
        self._refresh_routes()
        try:
            self._responses()
        except (ValueError, TypeError, KeyError, OverflowError) as exc:
            self.fault = str(exc)
        if self.transport.error:
            self.fault = self.transport.error
        if self.fault:
            self.phase = "fault"
        healthy = (not self.fault and self.transport.process.poll() is None
                   and wall - self.last_response_wall <= COMMAND_TTL)
        active = self.authorized and not self.stopped and not self._expired()
        if self._expired():
            self.phase = "expired"
        for robot_id, link in self.links.items():
            position, _ = self._pose(robot_id)
            velocity = link.velocity(position[:2], wall, active, healthy)
            self.data.ctrl[self.bindings[robot_id]["actuators"]] = velocity
            if not any(velocity):
                self._brake(robot_id)  # Ideal simulated brake, not physical stopping distance.
        self.elapsed_credit = min(.05, self.elapsed_credit + dt)
        timestep = float(self.model.opt.timestep)
        while self.elapsed_credit + 1e-9 >= timestep:
            self.mj.mj_step(self.model, self.data)
            self.elapsed_credit -= timestep
        self.mj.mj_forward(self.model, self.data)
        if not self.np.isfinite(self.data.qpos).all() or not self.np.isfinite(self.data.qvel).all():
            self.fault = "nonfinite MuJoCo state"
            self._brake_all()
            raise RuntimeError(self.fault)
        self._update_radio_positions()
        self._refresh_routes()
        if float(self.data.time) - self.last_scan_s >= .5:
            self._lidar()
        if float(self.data.time) - self.last_request_s >= .2 and not self.fault:
            self._request()
        self.last_tick_wall = wall

    def snapshot(self, map_node="hub"):
        if map_node not in self.mesh.maps:
            raise ValueError("unknown map replica")
        wall = time.monotonic()
        robots = []
        for robot_id, link in self.links.items():
            position, velocity = self._pose(robot_id)
            state = link.state
            if not link.available:
                state = "paused"
            if self.fault:
                state = "watchdog_stop"
            robots.append({"id": robot_id, "name": link.config["name"], "color": link.config["color"],
                "position": position, "velocity": velocity, "online": link.online,
                "direct_link_up": self.mesh.edges[self.mesh.edge_id(robot_id, "hub")]["up"],
                "direct_link_enabled": self.mesh.edges[self.mesh.edge_id(robot_id, "hub")]["enabled"],
                "route": list(self._routes[robot_id]),
                "available": link.available, "state": state, "job_id": link.job_id,
                "path": copy.deepcopy(link.path[link.cursor:] if not link.online else link.path),
                "lidar_points": link.lidar_points, "lidar_rays": link.lidar_rays,
                "local_voxels": len(link.local_map.cells), "delivered_telemetry": copy.deepcopy(link.delivered),
                "peer_voxels": len(self.mesh.maps[robot_id].cells),
                "last_telemetry_route": list(getattr(link, "last_telemetry_route", [])),
                "last_command_route": list(getattr(link, "last_command_route", [])),
                "network": dict(link.stats)})
        return {"schema": "wendy.collaborative-office.runtime.v1", "run_id": self.run_id,
            "sequence": self.sequence, "time_s": float(self.data.time), "clock_domain": "simulation:" + self.run_id,
            "synthetic": True, "phase": self.phase, "authorized": self.authorized,
            "stopped": self.stopped, "fault": self.fault, "robots": robots,
            "route_exclusions":copy.deepcopy(self.exclusions),
            "route_exclusions_frozen":self.authorization_started_s is not None,
            "route_preview":copy.deepcopy(self.route_preview),
            "preview_request":copy.deepcopy(self.preview_request),
            "jobs": copy.deepcopy(self.jobs), "completed": self.completed, "total": len(self.config["jobs"]),
            "events": list(self.events), "voxels": self.mesh.maps[map_node].snapshot(), "voxel_size": VOXEL_SIZE,
            "map_node": map_node,
            "map_semantics": "Accumulated occupied ray hits; unobserved space is unknown; no free-space inference",
            "network": self.mesh.snapshot(),
            "coordinator_age_s": max(0, wall - self.last_response_wall) if math.isfinite(self.last_response_wall) else None}

    def close(self):
        if not self.closed:
            self._brake_all()
            self.transport.close()
            self.closed = True
