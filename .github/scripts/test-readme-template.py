#!/usr/bin/env python3
"""Generate the README's real pinned template, compile it, and check HTTP output.

This executes init and the generated app on the test host. It does not execute
wendy run, build an OCI image, discover devices or claim remote deployment.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[2]
PIN = '44d5d1ba804c332c1b6787b9812bbcdd4e005aee'
LOCK = ROOT / '.github/scripts/fixtures/readme-template/package-lock.json'
SPEC = importlib.util.spec_from_file_location('readme_owner', ROOT / '.github/scripts/run-owned-media-test.py')
OWNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNER)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, url):
        raise RuntimeError('generated app unexpectedly redirected the request')


def exercise(project, mode):
    # No process-global monkey patch changes binding or application handlers.
    # This preload only records the actual net.Server 'listening' event with
    # the owned child's PID and nonce, excluding a foreign port's response.
    record, preload = project / 'listener.json', project / 'observe-listen.mjs'
    nonce = uuid.uuid4().hex
    preload.write_text('''import {Server} from 'node:net';
import fs from 'node:fs';
const original = Server.prototype.emit;
Server.prototype.emit = function(event, ...args) {
  if (event === 'listening') {
    const record=process.env.README_LISTENER_RECORD;
    fs.writeFileSync(record+'.tmp',
      JSON.stringify({nonce:process.env.README_LISTENER_NONCE,pid:process.pid,address:this.address()}), {flag:'wx'});
    fs.renameSync(record+'.tmp',record);
  }
  return original.call(this,event,...args);
};
''')
    record.unlink(missing_ok=True)
    # Refuse an occupied application port, including another user's service.
    with socket.socket() as reservation:
        reservation.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        reservation.bind(('0.0.0.0', 5001))
    env = dict(os.environ, README_LISTENER_RECORD=str(record), README_LISTENER_NONCE=nonce)
    child = None
    interrupted = 0

    def stop(signum, frame):
        nonlocal interrupted
        interrupted = signum

    previous = {sig: signal.signal(sig, stop) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        with (project / (mode + '-server.log')).open('wb') as log:
            child = subprocess.Popen(['node', '--import', str(preload), 'dist/index.js'], cwd=project,
                                     env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 10
            while not record.exists():
                require(not interrupted and child.poll() is None and time.monotonic() < deadline,
                        'generated server did not own its listening socket')
                time.sleep(.03)
            observed = json.loads(record.read_text())
            require(observed['nonce'] == nonce and observed['pid'] == child.pid and
                    observed['address']['port'] == 5001 and child.poll() is None,
                    'listener receipt belongs to another process')
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
            with opener.open('http://127.0.0.1:5001/?fixture=' + nonce, timeout=2) as response:
                require(response.status == 200 and response.geturl().startswith('http://127.0.0.1:5001/'),
                        'unexpected generated app response')
                body = response.read(4097)
            require(not interrupted and child.poll() is None, 'server ended before response verification')
            require(json.loads(body) == {'message': 'hello-world'}, 'README_HTTP_BODY: generated app contradicts README')
            print('README_GENERATED_HTTP_VERIFIED exact hello-world from owned Node process', flush=True)
    finally:
        try:
            if child is not None:
                if child.poll() is None:
                    child.terminate()
                try:
                    child.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=3)
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    require(not interrupted, 'generated app fixture was interrupted')


def run(checkout, evidence):
    require(sys.platform in ('linux', 'darwin'), 'Unix software fixture only')
    evidence.mkdir(parents=True, exist_ok=False)
    with tempfile.TemporaryDirectory(prefix='wendy-readme-template-') as temporary:
        work = Path(temporary).resolve()
        archive = work / 'templates.tar.gz'
        with (evidence / 'archive.log').open('wb') as log:
            code = OWNER.run(['git', '-C', str(checkout.resolve()), 'archive', '--format=tar.gz',
                              '--prefix=templates-fixture/', '-o', str(archive), PIN, 'meta.json', 'node/simple-api'],
                             60, stdout=log)
        require(code == 0, 'pinned template archive failed')
        with tarfile.open(archive) as source:
            for member in source.getmembers():
                require((member.name.startswith('templates-fixture/') or
                         (member.name == 'templates-fixture' and member.isdir())) and '..' not in Path(member.name).parts
                        and (member.isdir() or member.isfile()), 'unexpected pinned archive member')
            source.extractall(work, filter='data')
        generated, home, cache = work / 'generated', work / 'home', work / 'npm-cache'
        for directory in (generated, home, cache):
            directory.mkdir()
        config = home / '.npmrc'
        config.write_text('registry=https://registry.npmjs.org/\n')
        env = dict(os.environ, WENDY_DOCS_TEMPLATE_CHECKOUT=str(work / 'templates-fixture'),
                   WENDY_DOCS_TEMPLATE_ARCHIVE=str(archive), WENDY_DOCS_TEMPLATE_WORK=str(generated),
                   WENDY_CONFIG_DIR=str(home / '.wendy'), WENDY_SECRET_STORE='file',
                   WENDY_ANALYTICS='false', WENDY_NO_UPDATE_CHECK='1', CI='1')
        with (evidence / 'generate.log').open('wb') as log:
            code = OWNER.run(['go', 'test', '-tags', 'regression_docs_fixture', './go/internal/cli/commands',
                              '-run', '^TestREADMEInitPublishedTemplateRuntime$', '-count=1', '-timeout=60s', '-v'],
                             300, cwd=ROOT, env=env, stdout=log)
        text = (evidence / 'generate.log').read_text()
        require(code == 0 and 'README_INIT_GENERATED_VERIFIED' in text and '--- SKIP:' not in text,
                'README init did not execute successfully')
        project = generated / 'hello-wendy'
        (project / 'package-lock.json').write_bytes(LOCK.read_bytes())
        env = {'PATH': os.environ['PATH'], 'HOME': str(home), 'TMPDIR': str(work), 'CI': '1',
               'npm_config_cache': str(cache), 'npm_config_userconfig': str(config),
               'npm_config_registry': 'https://registry.npmjs.org/'}
        for label, command in [('dependencies', ['npm', 'ci', '--ignore-scripts', '--no-audit', '--no-fund']),
                               ('build', ['npm', 'run', 'build'])]:
            with (evidence / (label + '.log')).open('wb') as log:
                code = OWNER.run(command, 120, cwd=project, env=env, stdout=log)
            require(code == 0, label + ' failed; see owned fixture log')
        javascript = project / 'dist/index.js'
        original = javascript.read_text()
        require(original.count('hello-world') == 1, 'generated handler control boundary changed')
        results = {}
        for mode in ('normal', 'wrong-response'):
            javascript.write_text(original if mode == 'normal' else original.replace('hello-world', 'wrong-response'))
            with (evidence / (mode + '.log')).open('wb') as log:
                code = OWNER.run([sys.executable, str(Path(__file__).resolve()), '--exercise', str(project), mode],
                                 25, cwd=project, env=env, stdout=log)
            text = (evidence / (mode + '.log')).read_text()
            (evidence / (mode + '-server.log')).write_bytes((project / (mode + '-server.log')).read_bytes())
            if mode == 'normal':
                require(code == 0 and 'README_GENERATED_HTTP_VERIFIED' in text, 'normal generated response failed')
            else:
                require(code == 1 and 'RuntimeError: README_HTTP_BODY:' in text and
                        'README_GENERATED_HTTP_VERIFIED' not in text, 'wrong-body control missed intended assertion')
            results[mode] = {'exit_code': code, 'expected': True}
        report = {'status': 'passed', 'templates_revision': PIN, 'archive_sha256': sha(archive),
                  'readme_sha256': sha(ROOT / 'README.md'), 'package_lock_sha256': sha(LOCK),
                  'generated_source_sha256': sha(project / 'src/index.ts'), 'results': results,
                  'fixture_sources': {name: sha(ROOT / name) for name in (
                      '.github/scripts/test-readme-template.py', '.github/scripts/run-owned-media-test.py',
                      'go/internal/cli/commands/docs_readme_init_runtime_test.go')},
                  'scope': 'Actual README init handlers and pinned generated TypeScript HTTP app; no OCI/device deployment'}
        (evidence / 'evidence.json').write_text(json.dumps(report, indent=2) + '\n')
        print('README_TEMPLATE_EXECUTION_VERIFIED')


if __name__ == '__main__':
    if sys.argv[1:2] == ['--exercise']:
        exercise(Path(sys.argv[2]), sys.argv[3])
    else:
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument('--templates', required=True, type=Path)
        parser.add_argument('--evidence', required=True, type=Path)
        options = parser.parse_args()
        run(options.templates, options.evidence)
