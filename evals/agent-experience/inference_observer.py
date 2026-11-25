"""Loopback-only transparent inference observer. Stores sizes, counts and timings, never prompt text."""
import hashlib
import http.client
import json
import math
import platform
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


def payload_sizes(payload):
    def size(value):
        return len(json.dumps(value, ensure_ascii=False, separators=(',', ':')).encode())
    roles = {}
    for message in payload.get('messages', []):
        role = message.get('role', 'unknown')
        roles[role] = roles.get(role, 0) + size(message)
    return {'message_bytes_by_role': roles, 'tool_schema_bytes': size(payload.get('tools', [])),
            'request_bytes': size(payload)}


class InferenceObserver:
    def __init__(self, upstream_port, output, port=0):
        observer = self
        self.output = Path(output)
        self.lock = threading.Lock()
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                if self.path != '/v1/chat/completions':
                    self.send_error(404)
                    return
                started = time.time()
                tick = time.monotonic()
                record = {'started_at': started, 'usage': None, 'timings': None, 'complete': False}
                upstream = http.client.HTTPConnection('127.0.0.1', upstream_port, timeout=3600)
                replied = False
                try:
                    body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                    payload = json.loads(body)
                    record.update(payload_sizes(payload))
                    record['model'] = payload.get('model')
                    upstream.request('POST', self.path, body, {'Content-Type': 'application/json'})
                    response = upstream.getresponse()
                    record['http_status'] = response.status
                    self.send_response(response.status)
                    self.send_header('Content-Type', response.getheader('Content-Type', 'application/json'))
                    self.send_header('Connection', 'close')
                    self.end_headers()
                    replied = True
                    self.close_connection = True
                    if 'text/event-stream' in response.getheader('Content-Type', ''):
                        done = False
                        while line := response.readline():
                            if line.startswith(b'data: '):
                                data = line[6:].strip()
                                if data == b'[DONE]':
                                    done = True
                                else:
                                    try:
                                        chunk = json.loads(data)
                                        if chunk.get('usage') is not None:
                                            record['usage'] = chunk['usage']
                                        if chunk.get('timings') is not None:
                                            record['timings'] = chunk['timings']
                                        for choice in chunk.get('choices', []):
                                            delta = choice.get('delta') or {}
                                            if any(delta.get(k) for k in ('content', 'reasoning_content', 'tool_calls')):
                                                record.setdefault('first_output_seconds', time.monotonic() - tick)
                                    except ValueError:
                                        record['invalid_stream'] = True
                            self.wfile.write(line)
                            self.wfile.flush()
                        record['complete'] = response.status == 200 and done and not record.get('invalid_stream')
                    else:
                        body = response.read()
                        self.wfile.write(body)
                        data = json.loads(body)
                        record.update(usage=data.get('usage'), timings=data.get('timings'), complete=response.status == 200)
                except (OSError, ValueError, http.client.HTTPException) as error:
                    # Don't persist exception text: it may contain response or request data.
                    record['error_kind'] = type(error).__name__
                    if not replied:
                        self.send_error(502)
                finally:
                    upstream.close()
                    record['request_seconds'] = time.monotonic() - tick
                    usage = record['usage']
                    counts = ([usage.get('prompt_tokens', usage.get('input_tokens')),
                               usage.get('completion_tokens', usage.get('output_tokens'))]
                              if isinstance(usage, dict) else [])
                    record['usage_complete'] = record['complete'] and len(counts) == 2 and all(
                        isinstance(n, (int, float)) and not isinstance(n, bool) and math.isfinite(n) and n >= 0
                        for n in counts)
                    with observer.lock:
                        with observer.output.open('a') as stream:
                            stream.write(json.dumps(record) + '\n')
        self.server = ThreadingHTTPServer(('127.0.0.1', port), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def base_url(self):
        return f'http://127.0.0.1:{self.server.server_port}/v1'

    def start(self):
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


def file_sha256(path):
    try:
        with Path(path).open('rb') as stream:
            digest = hashlib.sha256()
            for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(chunk)
        return digest.hexdigest()
    except OSError:
        return None


def hardware_identity():
    facts = {'architecture': platform.machine(), 'kernel': platform.release()}
    for key, path in (('cpu', '/proc/cpuinfo'), ('memory', '/proc/meminfo')):
        try:
            lines = Path(path).read_text().splitlines()
            if key == 'memory':
                facts[key] = next((line for line in lines if line.startswith('MemTotal:')), None)
            else:
                facts[key] = sorted(set(line.strip() for line in lines if line.startswith(('model name', 'CPU part', 'Hardware'))))
        except OSError:
            facts[key] = None
    try:
        result = subprocess.run(['nvidia-smi', '--query-gpu=name,memory.total,driver_version', '--format=csv,noheader'],
                                capture_output=True, text=True, timeout=10)
        facts['gpu'] = result.stdout.strip() if result.returncode == 0 else None
    except (OSError, subprocess.SubprocessError):
        facts['gpu'] = None
    return facts


def identity(pin, context, argv, binary, facts, props=None):
    """An exact server binary and model pin identify embedded templates/default sampling settings."""
    props = props or {}
    binary_hash = file_sha256(binary)
    template = props.get('chat_template')
    return {'model': {'repo': pin['repo'], 'revision': pin['revision'], 'files': pin['files'],
                      'quantization': pin.get('quantization'), 'context': context},
            'server': {'binary_sha256': binary_hash, 'argv': argv,
                       'chat_template_sha256': hashlib.sha256(template.encode()).hexdigest() if isinstance(template, str) else None,
                       'generation_settings': props.get('default_generation_settings')},
            'hardware': facts,
            'missing': [name for name, known in (('server binary', binary_hash),
                         ('chat template', template), ('generation defaults', props.get('default_generation_settings')),
                         ('quantization', pin.get('quantization')), ('CPU', facts.get('cpu')),
                         ('GPU', facts.get('gpu')), ('memory', facts.get('memory')),
                         ('architecture', facts.get('architecture'))) if not known]}
