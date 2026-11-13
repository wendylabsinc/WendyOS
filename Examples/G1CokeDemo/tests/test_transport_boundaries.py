"""Real HTTP/thread boundaries; never construct a robot backend or policy model."""
import contextlib
import http.client
import json
import socket
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace

from physical_io import fsm_bridge, service
from runtime.inference_client import InferenceClient, RemoteCamera
from runtime.async_vision import AsyncVisionBuffer, VisionFrame, VisionUnavailable


@contextlib.contextmanager
def serving(server):
    worker = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": .01})
    worker.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()
        worker.join(2)
        if worker.is_alive():
            raise AssertionError("owned HTTP accept loop did not stop")


class LostReplyHandler(BaseHTTPRequestHandler):
    """Peer accepts a real request and deliberately loses exactly its first reply."""
    protocol_version = "HTTP/1.1"
    timeout = 2

    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.receipts.append((self.path, body))
        if len(self.server.receipts) == 1:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.close_connection = True
            return
        self.reply({"accepted": True})

    def do_GET(self):
        self.server.reads += 1
        if self.server.reads == 1:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.close_connection = True
            return
        self.reply({"healthy": True})

    def reply(self, value):
        payload = json.dumps(value).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)
        self.close_connection = True


def lost_reply_peer():
    server = ThreadingHTTPServer(("127.0.0.1", 0), LostReplyHandler)
    server.daemon_threads = False
    server.receipts = []
    server.reads = 0
    return server


class TransportBoundaries(unittest.TestCase):
    def test_actual_control_listeners_bind_loopback_and_serve(self):
        for factory, runtime, path, expected in (
            (service.make_server, SimpleNamespace(state=lambda: {"healthy": True, "identity": "physical-fixture"}), "/health", {"healthy": True, "identity": "physical-fixture"}),
            (fsm_bridge.make_server, SimpleNamespace(read_fsm=lambda: {"fsm_id": 0, "fsm_mode": 3}), "/state", {"healthy": True, "fsm_id": 0, "fsm_mode": 3}),
        ):
            with self.subTest(factory=factory.__module__):
                server = factory(runtime, port=0)
                try:
                    self.assertEqual(server.socket.getsockname()[0], "127.0.0.1", "CONTROL_BOUND_LOOPBACK")
                except BaseException:
                    server.server_close()
                    raise
                with serving(server):
                    connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=2)
                    try:
                        connection.request("GET", path)
                        response = connection.getresponse()
                        self.assertEqual(response.status, 200)
                        self.assertEqual(json.loads(response.read()), expected)
                    finally:
                        connection.close()
                # Do not open a wildcard socket even if the guard regresses.
                # The default-bind control above proves the actual socket path.
                for host in ("localhost", "192.0.2.1"):
                    with self.assertRaisesRegex(ValueError, "loopback"):
                        factory(runtime, host=host, port=0)

    def test_post_lost_reply_is_not_replayed_and_get_can_reconnect(self):
        with serving(lost_reply_peer()) as server:
            client = InferenceClient(f"http://127.0.0.1:{server.server_port}")
            try:
                with self.assertRaises(http.client.RemoteDisconnected, msg="POST_NO_AUTOMATIC_REPLAY"):
                    client.request("POST", "/propose", {"sampled_at_ns": 42})
                self.assertEqual(server.receipts, [("/propose", {"sampled_at_ns": 42})])
                self.assertIsNone(client.connection)
                self.assertEqual(client.request("GET", "/health"), {"healthy": True})
                self.assertEqual(server.reads, 2)
            finally:
                client.close()

    def test_deactivate_lost_reply_retains_identity_until_explicit_retry(self):
        with serving(lost_reply_peer()) as server:
            client = InferenceClient(f"http://127.0.0.1:{server.server_port}")
            client.session_id = "owned-session-0123456789"
            camera = RemoteCamera(client)
            try:
                with self.assertRaises(http.client.RemoteDisconnected):
                    camera.deactivate()
                self.assertEqual(client.session_id, "owned-session-0123456789", "DEACTIVATE_RETAINS_IDENTITY")
                self.assertEqual(len(server.receipts), 1)
                camera.deactivate()
                self.assertIsNone(client.session_id)
                self.assertEqual(server.receipts, [("/deactivate", {"session_id": "owned-session-0123456789"})] * 2)
            finally:
                client.close()

    def test_stream_change_discards_cached_and_inflight_embeddings(self):
        encoding, release = threading.Event(), threading.Event()
        def encode(payload):
            if payload == "old-pending":
                encoding.set()
                if not release.wait(3):
                    raise AssertionError("owned encoder release deadline")
            return payload
        buffer = AsyncVisionBuffer(encode, maximum_capture_age_s=1, clock_ns=lambda: 100)
        buffer.start()
        try:
            buffer.submit(VisionFrame(1, "old", 100, "old-ready"))
            with buffer._condition:
                self.assertTrue(buffer._condition.wait_for(lambda: buffer._processed == 1, 2))
            old = buffer.latest()
            buffer.submit(VisionFrame(2, "old", 100, "old-pending"))
            self.assertTrue(encoding.wait(2))
            buffer.submit(VisionFrame(0, "new", 100, "new-image"))
            with self.assertRaises(VisionUnavailable, msg="STREAM_CHANGE_INVALIDATES_CACHED"):
                buffer.latest()
            with self.assertRaises(VisionUnavailable):
                buffer.admit(old, control_at_ns=100)
            release.set()
            with buffer._condition:
                self.assertTrue(buffer._condition.wait_for(lambda: buffer._processed == 2, 2))
            self.assertEqual(buffer.latest().embedding, "new-image")
            self.assertEqual(buffer.status()["discarded_on_episode_reset"], 1)
        finally:
            release.set()
            buffer.close()
            self.assertFalse(buffer._thread.is_alive(), "owned inference worker did not stop")

    def test_actual_physical_server_drains_active_request(self):
        entered, release, closed = threading.Event(), threading.Event(), threading.Event()
        def health():
            entered.set()
            if not release.wait(5):
                raise AssertionError("owned request release deadline")
            return {"healthy": True, "receipt": "after-release"}
        server = service.make_server(SimpleNamespace(state=health), port=0)
        connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=2)
        closer = None
        with serving(server):
            try:
                connection.request("GET", "/health")
                self.assertTrue(entered.wait(2))
                server.shutdown()
                def close():
                    server.server_close()
                    closed.set()
                closer = threading.Thread(target=close)
                closer.start()
                self.assertFalse(closed.wait(.1), "HTTP_DRAIN_WAITS_FOR_HANDLER")
                release.set()
                response = connection.getresponse()
                self.assertEqual(response.status, 200)
                self.assertEqual(json.loads(response.read()), {"healthy": True, "receipt": "after-release"})
                self.assertTrue(closed.wait(2))
            finally:
                release.set()
                connection.close()
                if closer is not None:
                    closer.join(3)
                    self.assertFalse(closer.is_alive(), "owned server closer did not stop")


if __name__ == "__main__":
    unittest.main(verbosity=2)
