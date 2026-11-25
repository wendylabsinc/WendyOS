import io
from http.client import HTTPConnection
import threading
import unittest
from unittest.mock import patch

from integration.distributedfusion import relay


class Response(io.BytesIO):
    code = 200


class RelayTests(unittest.TestCase):
    def test_actual_http_preserves_bytes_nonce_and_auth_only_for_read_paths(self):
        requests = []
        class Opener:
            def open(self, request, timeout):
                requests.append((request, timeout))
                return Response(b'{"original": "same bytes"}\n')
        with patch.object(relay, 'build_opener', return_value=Opener()):
            server = relay.server('127.0.0.1', 0, 'http://10.20.0.5:8091')
            thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
            try:
                for path in relay.PATHS:
                    connection = HTTPConnection('127.0.0.1', server.server_port, timeout=2)
                    connection.request('GET', path, headers={'Authorization': 'Bearer test-only', 'X-Wendy-Clock-Nonce': 'a' * 64})
                    reply = connection.getresponse()
                    self.assertEqual(reply.status, 200)
                    self.assertEqual(reply.read(), b'{"original": "same bytes"}\n')
                    connection.close()
                    request, timeout = requests[-1]
                    self.assertEqual(request.full_url, 'http://10.20.0.5:8091' + path)
                    self.assertEqual(request.get_header('Authorization'), 'Bearer test-only')
                    self.assertEqual(request.get_header('X-wendy-clock-nonce'), 'a' * 64)
                    self.assertEqual(timeout, 3)
                count = len(requests)
                for method, path in [('POST', '/v1/step'), ('GET', '/v1/captures/camera-a'), ('GET', '/v1/clock?other=1')]:
                    connection = HTTPConnection('127.0.0.1', server.server_port, timeout=2)
                    connection.request(method, path)
                    reply = connection.getresponse(); self.assertIn(reply.status, (404, 501)); reply.read(); connection.close()
                self.assertEqual(len(requests), count)
            finally:
                server.shutdown(); server.server_close(); thread.join(2)


if __name__ == '__main__':
    unittest.main()
