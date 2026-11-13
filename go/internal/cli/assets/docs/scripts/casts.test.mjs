import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { readdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

// Terminal recordings under casts/ are published on the docs site, and a
// recording captures whatever was on screen. screenshots/tighten-cast.py removes
// the recorder's own details; this test is the second check, on every recording
// that is committed, for things no recording should show.

const docsRoot = path.resolve(fileURLToPath(new URL('..', import.meta.url)));
const castsRoot = path.join(docsRoot, 'casts');

// Screen updates split text with escape codes, so checks run on plain text.
const escapes = /\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]/g;

const checks = [
  // /Users/you and /home/you are the placeholders; `wendy` is the device user.
  ['a home folder', /\/(?:Users|home)\/(?!(?:you|Shared|wendy)(?![A-Za-z0-9._-]))[A-Za-z0-9._-]+/],
  ['an email address', /[A-Za-z0-9._%+-]+@(?!example\.(?:com|org)\b)[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}/],
  ['a GitHub token', /\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})/],
  ['an AWS access key', /\bAKIA[0-9A-Z]{16}\b/],
  ['a Slack token', /\bxox[abprs]-[A-Za-z0-9-]{10,}/],
  ['an API key', /\bsk-[A-Za-z0-9_-]{20,}/],
  ['a JSON web token', /\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\./],
  ['a private key', /-----BEGIN [A-Z ]*PRIVATE KEY-----/],
  ['a bearer token', /\bBearer\s+[A-Za-z0-9._~+/-]{20,}/],
  ['a password or secret value', /\b(?:password|passwd|secret|api[_-]?key|token)\s*[=:]\s*["']?(?![<$*{]|x{3,})[^\s"']{8,}/i],
];
const allowed = new Set(['git@github.com']);

// Describe what a recording's text shows that it should not. Only the start of
// each match is reported, so a real credential is not copied into test output.
function findings(text) {
  const plain = text.replace(escapes, '');
  const found = [];
  for (const [label, pattern] of checks) {
    const match = plain.match(pattern);
    if (match && !allowed.has(match[0])) found.push(`${label} (${match[0].slice(0, 16)}...)`);
  }
  return found;
}

async function* castFiles(dir) {
  if (!existsSync(dir)) return;
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const entryPath = path.join(dir, entry.name);
    if (entry.isDirectory()) yield* castFiles(entryPath);
    else if (entry.name.endsWith('.cast')) yield entryPath;
  }
}

test('the recording checks catch personal paths and credentials', () => {
  const shown = [
    '/Users/alice/Documents/wendy-robot-tour',
    '/home/bob/project',
    'Signed in as alice@wendy.dev',
    'ghp_0123456789abcdefghijklmnopqrstuvwxyz',
    'AKIAABCDEFGHIJKLMNOP',
    'Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123',
    'password: hunter2hunter2',
    '\x1b[32m/Users/\x1b[0malice/split-by-colour',
  ];
  for (const text of shown) assert.notDeepEqual(findings(text), [], text);

  const fine = [
    '/Users/you/Documents/wendy-robot-tour/apps/patrol/patrol.py',
    '/home/wendy/app',
    'git clone git@github.com:wendylabsinc/wendyos.git',
    'Contact someone@example.com',
    'token: <your-token>',
    'Stop simulator: wendy vm stop tour-721d5180',
  ];
  for (const text of fine) assert.deepEqual(findings(text), [], text);
});

test('published recordings show no personal paths or credentials', async () => {
  const problems = [];
  for await (const file of castFiles(castsRoot)) {
    const name = path.relative(docsRoot, file);
    const lines = (await readFile(file, 'utf8')).split('\n').filter((line) => line.trim() !== '' && !line.startsWith('#'));
    const header = JSON.parse(lines[0]);
    // tighten-cast.py keeps only the size and title. A raw `asciinema rec` file
    // also stores the shell and command line, so it has not been through it.
    for (const key of ['env', 'command']) {
      if (key in header) problems.push(`${name}: header has "${key}"; run screenshots/tighten-cast.py`);
    }
    const output = lines
      .slice(1)
      .map((line) => JSON.parse(line))
      .filter(([, code]) => code === 'o')
      .map(([, , data]) => data)
      .join('');
    for (const found of findings(`${header.title ?? ''}\n${output}`)) problems.push(`${name}: shows ${found}`);
  }
  assert.deepEqual(problems, []);
});
