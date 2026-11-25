import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import tempfile
import threading
import unittest
import urllib.request

from inference_observer import InferenceObserver, identity, payload_sizes


class ObserverTests(unittest.TestCase):
    def serve(self, chunks, content_type='text/event-stream'):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.log = Path(tmp.name) / 'requests.jsonl'
        self.seen = []
        seen = self.seen
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_POST(self):
                seen.append(self.rfile.read(int(self.headers['Content-Length'])))
                self.send_response(200)
                self.send_header('Content-Type', content_type)
                self.end_headers()
                for chunk in chunks:
                    self.wfile.write(chunk)
                    self.wfile.flush()
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.observer = InferenceObserver(server.server_port, self.log)
        self.observer.start()
        self.addCleanup(self.observer.close)
        return self.observer.base_url + '/chat/completions'

    def request(self, url):
        body = json.dumps({'model': 'test', 'stream': True,
                           'messages': [{'role': 'user', 'content': 'private prompt'}],
                           'tools': [{'name': 'private_tool'}]}).encode()
        with urllib.request.urlopen(urllib.request.Request(url, body, {'Content-Type': 'application/json'})) as response:
            output = response.read()
        self.assertEqual(self.seen, [body])
        return output, json.loads(self.log.read_text())

    def test_stream_is_unchanged_and_request_statistics_are_private(self):
        chunks = [b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\n',
                  b'data: {"usage":{"prompt_tokens":30,"completion_tokens":4},"timings":{"predicted_per_second":12}}\n\n',
                  b'data: [DONE]\n\n']
        output, record = self.request(self.serve(chunks))
        self.assertEqual(output, b''.join(chunks))
        self.assertTrue(record['usage_complete'])
        self.assertEqual(record['timings']['predicted_per_second'], 12)
        self.assertGreater(record['request_seconds'], 0)
        self.assertGreaterEqual(record['first_output_seconds'], 0)
        self.assertNotIn('private prompt', self.log.read_text())
        self.assertNotIn('private_tool', self.log.read_text())
        self.assertNotIn('hello', self.log.read_text())

    def test_partial_stream_is_incomplete_even_with_usage(self):
        _, record = self.request(self.serve([b'data: {"usage":{"prompt_tokens":30,"completion_tokens":4}}\n\n']))
        self.assertFalse(record['complete'])
        self.assertFalse(record['usage_complete'])
        self.assertIsNone(record['timings'])

    def test_nonstreaming_json_passes_through(self):
        raw = b'{"usage":{"prompt_tokens":10,"completion_tokens":2},"choices":[]}'
        output, record = self.request(self.serve([raw], 'application/json'))
        self.assertEqual(output, raw)
        self.assertTrue(record['usage_complete'])

    def test_missing_usage_never_becomes_zero(self):
        _, record = self.request(self.serve([b'data: [DONE]\n\n']))
        self.assertFalse(record['usage_complete'])
        self.assertIsNone(record['usage'])

    def test_empty_usage_is_incomplete(self):
        _, record = self.request(self.serve([b'data: {"usage":{}}\n\n', b'data: [DONE]\n\n']))
        self.assertFalse(record['usage_complete'])

    def test_identity_changes_with_binary_model_and_template(self):
        with tempfile.TemporaryDirectory() as tmp:
            binary = Path(tmp) / 'server'
            binary.write_bytes(b'v1')
            pin = {'repo': 'repo', 'revision': 'abc', 'files': [{'sha256': 'x'}], 'quantization': 'Q4'}
            props = {'chat_template': 'template', 'default_generation_settings': {'temperature': .7}}
            before = identity(pin, 4096, ['server'], binary, {'device': 'thor', 'architecture': 'aarch64', 'cpu': ['ARM'], 'gpu': 'Thor', 'memory': '128GB'}, props)
            self.assertEqual(before['missing'], [])
            binary.write_bytes(b'v2')
            after = identity(pin, 4096, ['server'], binary, {'device': 'thor', 'architecture': 'aarch64', 'cpu': ['ARM'], 'gpu': 'Thor', 'memory': '128GB'}, props)
            self.assertNotEqual(before['server']['binary_sha256'], after['server']['binary_sha256'])
            props['chat_template'] = 'new template'
            self.assertNotEqual(after['server']['chat_template_sha256'], identity(pin, 4096, [], binary, {}, props)['server']['chat_template_sha256'])
            missing = identity(pin, 4096, [], '/missing', {})
            self.assertIn('chat template', missing['missing'])
            self.assertIn('server binary', missing['missing'])

    def test_payload_roles_remain_separate(self):
        sizes = payload_sizes({'messages': [{'role': 'system', 'content': 's'}, {'role': 'tool', 'content': 't'}]})
        self.assertEqual(set(sizes['message_bytes_by_role']), {'system', 'tool'})


if __name__ == '__main__':
    unittest.main()
