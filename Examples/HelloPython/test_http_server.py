import http.client
import json
import queue
import socket
import threading
import unittest
from http.server import ThreadingHTTPServer
from unittest.mock import patch

import app


class HTTPServerTest(unittest.TestCase):
    def setUp(self):
        started = queue.Queue()

        def create_server(address, handler):
            # Run the production entrypoint with an ephemeral loopback listener.
            server = ThreadingHTTPServer(("127.0.0.1", address[1]), handler)
            started.put(server)
            return server

        self.factory_patch = patch.object(app, "ThreadingHTTPServer", create_server)
        self.factory_patch.start()
        self.thread = threading.Thread(target=app.run_server, args=(0,), daemon=True)
        self.thread.start()
        self.server = started.get(timeout=2)
        self.clients = []

    def tearDown(self):
        for client in self.clients:
            client.close()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.factory_patch.stop()

    def connection(self):
        client = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=1)
        self.clients.append(client)
        return client

    def test_idle_keep_alive_does_not_block_another_client(self):
        first = self.connection()
        first.request("GET", "/health")
        response = first.getresponse()
        self.assertEqual(response.status, 200)
        response.read()
        self.assertIsNotNone(first.sock)

        second = self.connection()
        second.request("GET", "/api/hello")
        response = second.getresponse()
        self.assertEqual(response.status, 200)
        self.assertEqual(json.loads(response.read())["message"], "Hello World!")

    def test_incomplete_request_connection_expires(self):
        # Use the same handler with a shorter bound to avoid a 15-second test.
        with patch.object(app.HelloWorldHandler, "timeout", 0.1):
            client = socket.create_connection(self.server.server_address, timeout=1)
            self.clients.append(client)
            client.sendall(b"GET /health HTTP/1.1\r\nHost: localhost\r\n")
            self.assertEqual(client.recv(1), b"")


if __name__ == "__main__":
    unittest.main()
