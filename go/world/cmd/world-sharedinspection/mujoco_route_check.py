"""Owned local MuJoCo geometry check; no deployment, task signing or media files.

The Go test reads actual Atlas state over authenticated loopback HTTP, then
sends its proposed route on stdin. This accelerated test follower exercises
the real planar physics/local obstacle sensor and captures four originals.
It is not a deployed mission/executor qualification.
"""
import json
import math
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from sim.shared_perception_scene import SharedPerceptionScene
from sim.shared_inspection import SharedInspection, PREFIX, INSPECTION_SEQUENCE_BASE


def main():
    scene = SharedPerceptionScene('local-navigation-check', inspection=True)
    control = SharedInspection(scene)
    render = None

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            if self.path != PREFIX+'state' or self.headers.get('Authorization') != 'Bearer '+'r'*32:
                self.send_error(403)
                return
            raw = json.dumps(control.state()).encode()
            self.send_response(200)
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        print(json.dumps({'url': 'http://127.0.0.1:'+str(server.server_port), 'run_id': scene.run_id,
                          'frame': scene.frame, 'package_sha256': scene.package_sha256,
                          'model_sha256': scene.model_sha256}), flush=True)
        request = json.loads(sys.stdin.readline())
        points, target = request['route_world'], request['target']
        index, sequence = 1, 0
        for _ in range(5000):
            state = control.state()
            if math.dist(state['position_m'], target) <= .05:
                break
            while index < len(points)-1 and math.dist(state['position_m'], points[index]) < .03:
                index += 1
            delta = [points[index][axis]-state['position_m'][axis] for axis in range(3)]
            distance = math.hypot(delta[0], delta[2])
            speed = min(.35, 3*distance)
            sequence += 1
            control.request(PREFIX+'command', {'session': control.session, 'sequence': sequence,
                            'velocity_mps': [delta[0]/distance*speed, 0, delta[2]/distance*speed], 'lease_ms': 300})
            with control.lock:
                control._tick(time.monotonic())
        else:
            raise AssertionError('actual local route did not reach the destination')
        control.request(PREFIX+'stop', {'session': control.session, 'sequence': sequence+1})
        state = control.state()
        assert math.dist(state['position_m'], target) <= .05 and state['velocity_mps'] == [0., 0., 0.]
        assert state['obstacle_clear'] and sum(w.number for w in scene.data.warning) == 0
        render = control.render_copy()
        state, _, _ = control.copy_state(render)
        snapshot = control.snapshot(render, state, INSPECTION_SEQUENCE_BASE)
        print(json.dumps(snapshot, separators=(',', ':')), flush=True)
    finally:
        server.shutdown()
        server.server_close()
        thread.join()
        if render is not None:
            render.close()
        control.close()
        scene.close()


if __name__ == '__main__':
    main()
