import assert from 'node:assert/strict';
import test from 'node:test';
import { commandFlags, commandPages, commandRoute, renderCommand } from './cli-reference.mjs';

const commands = [
  { path: 'wendy', use: 'wendy', persistent_flags: [{ name: 'device', type: 'string', usage: 'Target' }] },
  { path: 'wendy device', use: 'device', persistent_flags: [{ name: 'timeout', type: 'int', default: '10' }] },
  { path: 'wendy device info', use: 'info', local_flags: [{ name: 'timeout', type: 'int', default: '30', usage: 'Use <seconds> | {auto}' }] },
];

test('inherits flags from every ancestor and honors the local override', () => {
  const flags = commandFlags(commands[2], commands);
  assert.deepEqual(flags.map((f) => f.name), ['device', 'timeout']);
  assert.equal(flags[1].default, '30');
});

test('renders predictable reference sections with MDX-safe flag text', () => {
  const doc = renderCommand(commands[2], commands, '/docs/advanced/clients/wendy-cli/commands/device/info');
  assert.deepEqual([...doc.matchAll(/^## (.+)$/gm)].map((m) => m[1]), [
    'Syntax', 'Examples', 'Arguments', 'Flags', 'Output', 'JSON output', 'Exit codes', 'Common errors', 'See also', 'Details',
  ]);
  assert.match(doc, /wendy device info \[flags\]/);
  assert.match(doc, /&lt;seconds&gt; \\\| &#123;auto&#125;/);
  assert.match(doc, /\/docs\/reference\/cli\/device/);
});

test('maps command groups and leaf commands to stable routes', () => {
  assert.equal(commandRoute('wendy'), 'reference/cli');
  assert.equal(commandRoute('wendy device info'), 'reference/cli/device/info');
  assert.match(renderCommand(commands[0], commands), /\[wendy device\]\(\/docs\/reference\/cli\/device\)/);
});

test('leaf commands are direct links while command groups remain expandable', () => {
  assert.deepEqual(commandPages(commands[0], commands), ['index', 'device']);
  assert.deepEqual(commandPages(commands[1], commands), ['index', '...info']);
  assert.deepEqual(commandPages(commands[2], commands), ['index']);
});
