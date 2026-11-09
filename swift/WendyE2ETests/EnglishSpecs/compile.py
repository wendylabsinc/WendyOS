#!/usr/bin/env python3
"""Compile the four fixed English specs, or import a recorded compiler response."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import time

BASE = Path(__file__).resolve().parent
NAMES = ('CompilerInput.md', 'cache-clear.md', 'entitlement-add.md',
         'entitlement-invalid.md', 'device-default.md')
FILES = ('EnglishCacheClearTests.swift', 'EnglishEntitlementAddTests.swift',
         'EnglishEntitlementInvalidTests.swift', 'EnglishDeviceDefaultTests.swift')
ENVELOPE = """You are a blind English-to-Swift compiler. Compile the five input documents below. They are the only task and behavior inputs. Do not call any tool, read any file, run another model or CLI, inspect any implementation, or run tests. All input bytes are provided in this prompt. Return only Swift source with no Markdown fences or explanatory prose.

Output-envelope instruction (transport only): the compiler contract requests four source files. Serialize their contents into one response in the requested order. Immediately before each file, output exactly one Swift comment marker in this form, using that file's requested basename:
// FILE: EnglishCacheClearTests.swift
// FILE: EnglishEntitlementAddTests.swift
// FILE: EnglishEntitlementInvalidTests.swift
// FILE: EnglishDeviceDefaultTests.swift
Each marker starts its corresponding file, so place the next marker only after the preceding file's complete contents. Include each file's imports inside its own marked section. This envelope changes no behavior requirement. Do not add any other filenames or sections.
"""


def sha(payload):
    return hashlib.sha256(payload).hexdigest()


def inspect_generation(events, exit_status):
    acceptance = {'completed': False, 'no_tools': True, 'no_failures': True,
                  'process_succeeded': exit_status == 0}
    details = {'acceptance': acceptance}
    for line in events.read_text().splitlines():
        event = json.loads(line)
        if event.get('type') == 'thread.started':
            details['session_id'] = event.get('thread_id')
        if event.get('type') == 'turn.completed':
            acceptance['completed'] = True
            details['token_usage'] = event.get('usage')
        if event.get('type') in ('turn.failed', 'error'):
            acceptance['no_failures'] = False
        item = event.get('item', {})
        if item.get('type') not in (None, 'agent_message', 'reasoning', 'error'):
            acceptance['no_tools'] = False
        if item.get('type') == 'error':
            # The gated wrapper emits this warning while retaining hooks.
            if not item.get('message', '').startswith('`--dangerously-bypass-hook-trust` is enabled.'):
                acceptance['no_failures'] = False
    return details


def accepted(record):
    state = record.get('acceptance', {})
    return isinstance(state, dict) and all(state.get(key) is True for key in
        ('completed', 'no_tools', 'no_failures', 'process_succeeded'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-dir', required=True, type=Path,
                        help='New directory for prompt, response and measurements')
    parser.add_argument('--codex', default='codex', help='Codex executable or gated wrapper')
    parser.add_argument('--import-raw', type=Path,
                        help='Import an existing response without invoking a model')
    parser.add_argument('--cache-only', action='store_true',
                        help='Regenerate only the maintained cache case')
    args = parser.parse_args()
    out = args.run_dir.resolve()
    out.mkdir(parents=True, exist_ok=False)
    names, files, envelope = NAMES, FILES, ENVELOPE
    if args.cache_only:
        names, files = ('CacheCompilerInput.md', 'cache-clear.md'), FILES[:1]
        envelope = ('You are a blind English-to-Swift compiler. Compile the two input '
                    'documents below, which are the only task and behavior inputs. Do not '
                    'call any tool, read any file, inspect original or previously generated '
                    'source, run another model or CLI, or run tests. All input bytes are '
                    'provided in this prompt. Return only the requested complete Swift '
                    'source file, beginning with the exact marker required by '
                    'CacheCompilerInput.md, without Markdown fences or explanatory prose.\n')
    parts = [envelope]
    inputs = {}
    for name in names:
        payload = (BASE / name).read_bytes()
        inputs[name] = sha(payload)
        parts.append('\n=== BEGIN INPUT: ' + name + ' ===\n' + payload.decode('utf-8')
                     + '\n=== END INPUT: ' + name + ' ===\n')
    prompt = ''.join(parts).encode('utf-8')
    (out / 'prompt.txt').write_bytes(prompt)
    raw = out / 'raw-output.swift'
    record = {'input_sha256': inputs, 'prompt_sha256': sha(prompt),
              'model': 'gpt-6.1-sol', 'reasoning_effort': 'high'}
    if args.import_raw:
        source = json.loads(args.import_raw.with_name('measurement.json').read_text())
        payload = args.import_raw.read_bytes()
        if source.get('input_sha256') != inputs or source.get('raw_output_sha256') != sha(payload):
            raise SystemExit('Recorded output provenance does not match the current inputs or raw bytes')
        if 'acceptance' not in source:
            # Archived blind v1/v2 responses predate this field. Reconstruct it
            # from their actual adjacent events, never from hashes/exit alone.
            source.update(inspect_generation(args.import_raw.with_name('events.jsonl'),
                                             source.get('exit_status')))
        if not accepted(source):
            raise SystemExit('Recorded compiler generation was not accepted; replay rejected')
        record['source_measurement'] = source
        record['acceptance'] = source['acceptance']
        raw.write_bytes(payload)
        record['operation'] = 'Import recorded output; no model invocation'
    else:
        # A fresh process receives only the five documents on stdin. Disable
        # project document loading and keep its working directory in artifacts.
        command = [args.codex, 'exec', '--model', 'gpt-6.1-sol', '--config',
                   'model_reasoning_effort="high"', '--config', 'project_doc_max_bytes=0',
                   '--config', 'developer_instructions=""', '--ignore-user-config',
                   '--ignore-rules', '--ephemeral', '--dangerously-bypass-approvals-and-sandbox',
                   '--skip-git-repo-check', '--json', '--color', 'never', '--cd', str(out),
                   '--output-last-message', str(raw), '-']
        record['command'] = command
        started = time.monotonic()
        with (out / 'events.jsonl').open('wb') as stdout, (out / 'stderr.log').open('wb') as stderr:
            result = subprocess.run(command, cwd=out, input=prompt, stdout=stdout, stderr=stderr)
        record['wall_seconds'] = time.monotonic() - started
        record['exit_status'] = result.returncode
        record.update(inspect_generation(out / 'events.jsonl', result.returncode))
        if raw.exists():
            record['raw_output_sha256'] = sha(raw.read_bytes())
        (out / 'measurement.json').write_text(json.dumps(record, indent=2) + '\n')
        if not accepted(record):
            raise SystemExit('Compiler generation was not accepted; inspect recorded events')

    payload = raw.read_bytes()
    markers = list(re.finditer(rb'^// FILE: ([A-Za-z]+\.swift)\r?\n', payload, re.MULTILINE))
    if (b'```' in payload or not markers or markers[0].start() != 0
            or [m[1].decode() for m in markers] != list(files)):
        raise SystemExit('Compiler output has an invalid file envelope')
    destination = BASE.parent / 'Tests/WendyE2ETests/GeneratedEnglishSpecs'
    staged = out / 'staged'
    staged.mkdir()
    record['raw_output_sha256'] = sha(payload)
    record['files'] = {}
    for index, marker in enumerate(markers):
        end = markers[index + 1].start() if index + 1 < len(markers) else len(payload)
        # Keep the FILE comment too: each raw slice is copied byte-for-byte.
        content = payload[marker.start():end]
        path = staged / files[index]
        path.write_bytes(content)
        record['files'][path.name] = {'raw_slice_sha256': sha(content)}
    subprocess.run(['swift', 'format', '--configuration', str(BASE.parents[2] / '.swift-format'),
                    '--in-place', *[str(staged / name) for name in files]], check=True)
    for name in files:
        record['files'][name]['formatted_sha256'] = sha((staged / name).read_bytes())
    destination.mkdir(exist_ok=True)
    for name in files:
        (staged / name).replace(destination / name)
    for metadata in ('measurement.json', 'import.json'):
        (out / metadata).write_text(json.dumps(record, indent=2) + '\n')


if __name__ == '__main__':
    main()
