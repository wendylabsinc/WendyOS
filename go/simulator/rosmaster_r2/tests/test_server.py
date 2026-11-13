"""Exercise the browser's burst of connections before the server accepts them."""

import socket
import threading
import unittest

from r2_sim.server import Handler, SimulatorHTTPServer


class ServerTests(unittest.TestCase):
    def test_browser_connection_burst_loads_assets(self):
        server = SimulatorHTTPServer(('127.0.0.1', 0), Handler)
        server.runtime = None
        sockets = []
        thread = None
        try:
            # Model preconnections from several tabs arriving in one burst.
            # The old five-entry listen queue drops these before do_GET runs.
            for _ in range(32):
                sockets.append(socket.create_connection(server.server_address, timeout=.5))
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            for connection in sockets:
                connection.sendall(b'GET /robot-model.js HTTP/1.0\r\nHost: localhost\r\n\r\n')
            for connection in sockets:
                connection.settimeout(3)
                with connection.makefile('rb') as response:
                    payload = response.read()
                self.assertTrue(payload.startswith(b'HTTP/1.1 200 OK'))
                self.assertIn(b'export function createRobot', payload)
        finally:
            for connection in sockets:
                connection.close()
            if thread:
                server.shutdown()
                thread.join()
            server.server_close()
