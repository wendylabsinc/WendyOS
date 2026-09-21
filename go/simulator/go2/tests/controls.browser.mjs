// No simulator is needed. The real sandbox page uses mocked status responses.
// node tests/controls.browser.mjs [path/to/playwright/index.mjs]
// PLAYWRIGHT_MODULE may also point to an existing Playwright installation.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const { chromium } = await import(process.argv[2] || process.env.PLAYWRIGHT_MODULE || 'playwright');
const html = await readFile(new URL('../go2_sim/index.html', import.meta.url), 'utf8');
const origin = 'http://go2-controls.test';
const browser = await chromium.launch({ headless: true, channel: 'chromium' });
const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
const errors = [], posts = [];
page.on('pageerror', error => errors.push(error.message));
let statusPolls = 0;
const fixture = {
  armed: false,
  metrics: { real_time_factor: 1, camera_frames: 15, wall_seconds: 1, policy_p95_ms: 2 },
  ros_commands: { owner: null, sources: [] },
  sensor_settings: { lidar_enabled: true, camera_enabled: true, lidar_dropout: 0 },
};
const first = { publisher_gid: '11111111111111111111111111111111', kind: 'twist', node_name: 'wendy_go2_patrol', age_ms: 10, requires_restart: false };
const second = { publisher_gid: '22222222222222222222222222222222', kind: 'twist', node_name: 'wendy_go2_teleop', age_ms: 20, requires_restart: false };
const third = { publisher_gid: '33333333333333333333333333333333', kind: 'twist', age_ms: 30, requires_restart: false };

await page.route(`${origin}/**`, route => {
  const request = route.request(), path = new URL(request.url()).pathname;
  if (path === '/') return route.fulfill({ contentType: 'text/html', body: html });
  if (path === '/viewer.js') {
    // The controls regression does not need WebGL or simulation geometry.
    return route.fulfill({ contentType: 'text/javascript', body: `
      export class SandboxViewer {
        setActive() {} setFollowRobot() {} setLidarEnabled() {} resetView() {}
      }
    ` });
  }
  if (path === '/api/status') {
    return route.fulfill({ contentType: 'application/json', body: JSON.stringify({
      ...fixture, mode: `status-${++statusPolls}`,
    }) });
  }
  if (request.method() === 'POST') {
    posts.push({ path, body: request.postDataJSON() });
    return route.fulfill({ contentType: 'application/json', body: '{}' });
  }
  return route.fulfill({ status: 404 });
});

async function poll(count = 1) {
  const target = statusPolls + count;
  await page.waitForFunction(minimum =>
    Number(document.querySelector('#mode').textContent.split('-')[1]) >= minimum, target);
}

async function rememberOptions() {
  await page.evaluate(() => {
    const select = document.querySelector('#source');
    window.savedSourceOptions = new Map([...select.options].map(option => [option.value, option]));
  });
}

async function sameOptions(message) {
  assert(await page.evaluate(() => {
    const options = [...document.querySelector('#source').options];
    return options.length === window.savedSourceOptions.size && options.every(option =>
      window.savedSourceOptions.get(option.value) === option);
  }), message);
}

try {
  await page.goto(origin);
  await poll();
  const select = page.locator('#source');
  assert.equal(await select.locator('option').count(), 1);
  assert.equal(await select.inputValue(), '');
  await rememberOptions();
  await poll(3);
  await sameOptions('Repeated empty source lists must preserve the waiting option node');

  fixture.ros_commands.sources = [first, second];
  await poll();
  assert.equal(await select.locator('option').count(), 2);
  assert.deepEqual(await select.locator('option').allTextContents(), ['Patrol', 'Teleop']);
  assert.match(await select.locator('option').first().getAttribute('title'), /11111111111111111111111111111111/,
    'Friendly names must retain the full publisher ID in their tooltip');
  await rememberOptions();
  await select.focus();
  await select.selectOption(second.publisher_gid);
  assert.equal(await select.inputValue(), second.publisher_gid, 'The second publisher must be selectable');
  await poll(3);
  await sameOptions('Recurring status must preserve the exact source option nodes');
  assert.equal(await select.inputValue(), second.publisher_gid, 'Polling must retain the user selection');
  assert(await select.evaluate(element => element === document.activeElement), 'Polling must retain select focus');

  // Discovery order and changing sample ages do not identify a new publisher.
  fixture.ros_commands = { owner: first.publisher_gid, sources: [
    { ...second, age_ms: 200 }, { ...first, age_ms: 300 },
  ] };
  await poll(2);
  await sameOptions('Reordered status must retain the existing option nodes');
  assert.equal(await select.inputValue(), second.publisher_gid,
    'Status order and a different owner must not override the user selection');
  assert.equal(await page.locator('#owner').textContent(), 'Patrol has robot control.');

  fixture.ros_commands.sources = [third, first, second];
  await poll();
  await sameOptions('New discoveries must not change option nodes while the select has focus');
  assert.equal(await select.locator('option').count(), 2, 'Defer new publishers until the menu loses focus');
  await select.evaluate(element => element.blur());
  assert.equal(await select.locator('option').count(), 3, 'Show a pending publisher after the menu loses focus');
  const fallbackLabel = await select.locator(`option[value="${third.publisher_gid}"]`).textContent();
  assert.match(fallbackLabel, /^Velocity app \d+$/, 'Publishers without a node name need a readable fallback');
  assert.equal(await select.inputValue(), second.publisher_gid, 'Adding a source must retain the selection');
  await Promise.all([
    page.waitForResponse(response => new URL(response.url()).pathname === '/api/arm_ros'),
    page.click('#grant'),
  ]);
  assert.deepEqual(posts.find(request => request.path === '/api/arm_ros')?.body,
    { publisher_gid: second.publisher_gid }, 'Grant must use the selected publisher');

  await select.focus();
  await rememberOptions();
  fixture.ros_commands.sources = [first, { ...second, requires_restart: true }, third];
  await poll();
  const restarted = select.locator(`option[value="${second.publisher_gid}"]`);
  await sameOptions('A restart must not replace options inside a focused source menu');
  assert(!(await restarted.evaluate(option => option.disabled)), 'Defer option mutations until the menu loses focus');
  assert(await page.locator('#grant').isDisabled(), 'A pending restart must disable the grant immediately');
  await select.evaluate(element => element.blur());
  assert(await restarted.evaluate(option => option.disabled), 'A publisher requiring restart must become disabled');
  assert.match(await restarted.textContent(), /restart app/);
  assert.equal(await select.inputValue(), '', 'A revoked publisher must require a new explicit choice');
  await poll(2);
  assert.equal(await select.inputValue(), '', 'Later polls must not silently select another app');

  fixture.ros_commands.sources = [first, second, { ...third, kind: 'motion_switcher' }];
  await poll();
  assert(!(await restarted.evaluate(option => option.disabled)), 'A recovered publisher must become selectable');
  assert.doesNotMatch(await restarted.textContent(), /restart app/);
  assert(await select.locator(`option[value="${third.publisher_gid}"]`).evaluate(option => option.disabled),
    'Motion switcher discovery must not become a command grant target');
  assert.equal(await select.inputValue(), '', 'Publisher recovery must not restore a revoked selection');

  await select.selectOption(second.publisher_gid);
  await select.focus();
  await rememberOptions();
  fixture.ros_commands.sources = [first];
  await poll();
  await sameOptions('Publisher removal must wait until the source menu loses focus');
  assert(await page.locator('#grant').isDisabled(), 'Removing the selected publisher must disable its grant immediately');
  await select.evaluate(element => element.blur());
  assert.equal(await select.locator('option').count(), 1);
  assert.equal(await select.inputValue(), '', 'Removing the selected publisher must leave no grant target');
  await poll(2);
  assert.equal(await select.inputValue(), '', 'A removed selection must stay empty on later polls');
  fixture.ros_commands.sources = [third, first];
  await poll();
  assert.equal(await select.inputValue(), '', 'A new publisher must not bypass the explicit-choice requirement');
  assert.equal(await select.locator(`option[value="${third.publisher_gid}"]`).textContent(), fallbackLabel,
    'Fallback app names must stay stable when publishers disappear and return');

  fixture.ros_commands.sources = [first, second];
  await poll();
  await select.selectOption(second.publisher_gid);
  await select.evaluate(element => element.blur());
  fixture.ros_commands = { owner: second.publisher_gid, sources: [
    { ...first, age_ms: 1600 }, { ...second, age_ms: 1600 },
  ] };
  await poll();
  assert.deepEqual(await select.locator('option').evaluateAll(options => options.map(option => option.value)),
    [second.publisher_gid], 'Keep an active owner while removing expired non-owner sources');
  assert.equal(await select.inputValue(), second.publisher_gid);
  fixture.ros_commands.owner = null;
  await poll();
  assert.equal(await select.inputValue(), '');
  assert.equal(await select.locator('option').count(), 1);
  await rememberOptions();
  await poll(3);
  await sameOptions('Returning to an empty source list must retain one stable waiting option');

  await page.locator('details').evaluateAll(details => details.forEach(element => { element.open = true; }));
  await page.locator('#lidar-dropout').fill('65');
  await poll(2);
  assert.equal(await page.locator('#lidar-dropout').inputValue(), '65',
    'Sensor status must not overwrite a focused dropout edit');
  await page.locator('#obstacle-x').fill('3.7');
  await page.locator('#obstacle-y').fill('-2.1');
  await poll(2);
  assert.equal(await page.locator('#obstacle-x').inputValue(), '3.7');
  assert.equal(await page.locator('#obstacle-y').inputValue(), '-2.1',
    'Status must leave obstacle coordinates available for editing');

  assert.deepEqual(errors, [], 'The real sandbox controls must run without browser exceptions');
  console.log(JSON.stringify({ passed: true, statusPolls, checks: [
    'stable waiting option', 'stable publisher option nodes', 'user source selection',
    'reordered discovery', 'friendly source and owner names', 'selected publisher grant',
    'deferred focused source changes', 'source addition and expiry',
    'publisher restart and recovery', 'revoked selection remains empty', 'sensor and obstacle edits',
  ] }));
} finally {
  await browser.close();
}
