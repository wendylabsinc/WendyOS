import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('../ui/app.js', import.meta.url), 'utf8')
  .replace("import { mountViewer } from './viewer.js';", '');

function page(fetch) {
  const nodes = new Map();
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, {
      replaceChildren() {}, addEventListener() {},
      classList: { toggle() {} }, querySelector: () => ({}), children: [],
    });
    return nodes.get(id);
  };
  const context = vm.createContext({
    fetch, AbortSignal, setTimeout() {},
    mountViewer: () => ({ resetView() {}, setActive() {} }),
    document: { getElementById: node, addEventListener() {} },
  });
  vm.runInContext(source, context);
  return {
    command: () => vm.runInContext("command('run', { steps: 40 })", context),
    refresh: () => vm.runInContext('refresh()', context), node,
  };
}

const status = () => Response.json({ phase: 'idle', simulation_seconds: 0, total_steps: 40, metrics: {} });

test('configured HIL is selected initially without overwriting later controller choices', async () => {
  const app = page(async () => Response.json({
    phase: 'idle', hil_enabled: true, simulation_seconds: 0, total_steps: 40, metrics: {},
  }));
  await app.refresh();
  assert.equal(app.node('mode').value, 'hil');
  app.node('mode').value = 'expert';
  await app.refresh();
  assert.equal(app.node('mode').value, 'expert');
});

test('controls recover after a failed session request', async () => {
  let sessions = 0, commands = 0;
  const app = page(async (path, options) => {
    if (path === '/api/status') return status();
    if (path === '/api/session') {
      if (++sessions === 1) throw new TypeError('Failed to fetch');
      return Response.json({ token: 'recovered' });
    }
    commands++;
    assert.equal(options.headers['X-Coke-Token'], 'recovered');
    return Response.json({ accepted: true });
  });
  await app.command();
  assert.equal(commands, 0);
  assert.equal(app.node('error').textContent, 'Failed to fetch');
  await app.command();
  assert.equal(commands, 1);
});

test('commands fetch current tokens after a server restart', async () => {
  let token = 'before';
  const sent = [];
  const app = page(async (path, options) => {
    if (path === '/api/status') return status();
    if (path === '/api/session') return Response.json({ token });
    sent.push(options.headers['X-Coke-Token']);
    return Response.json({ accepted: true });
  });
  await app.command();
  token = 'after';
  await app.command();
  assert.deepEqual(sent, ['before', 'after']);
});

test('an uncertain POST failure is not automatically retried', async () => {
  let commands = 0;
  const app = page(async path => {
    if (path === '/api/status') return status();
    if (path === '/api/session') return Response.json({ token: 'current' });
    commands++;
    throw new TypeError('Failed to fetch');
  });
  await app.command();
  assert.equal(commands, 1);
});

test('manual controller choice during loading survives HIL discovery', async () => {
  let phase = 'loading';
  const app = page(async () => Response.json({
    phase, hil_enabled: true, simulation_seconds: 0, total_steps: 40, metrics: {},
  }));
  await app.refresh();
  app.node('mode').value = 'expert';
  app.node('steps').options = [{}];
  app.node('mode').onchange();
  phase = 'idle';
  await app.refresh();
  assert.equal(app.node('mode').value, 'expert');
});
