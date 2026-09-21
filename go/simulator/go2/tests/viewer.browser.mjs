// Run against a disposable local simulator (this test resets and moves its robot):
// GO2_RENDER=1 GO2_PORT=8899 .venv/bin/python -m go2_sim.server
// node tests/viewer.browser.mjs http://127.0.0.1:8899
// Requires Playwright + Chromium; PLAYWRIGHT_MODULE may point to a local install.
import assert from 'node:assert/strict';
const { chromium } = await import(process.argv[3] || process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.argv[2] || 'http://127.0.0.1:8899';
assert(['localhost', '127.0.0.1', '[::1]'].includes(new URL(url).hostname), 'Use a disposable local simulator');
const browser = await chromium.launch({ headless: true, channel: 'chromium' });
const context = await browser.newContext({ viewport: { width: 1280, height: 1100 } });
const page = await context.newPage();
page.setDefaultTimeout(20000);
const errors = [], requests = [];
page.on('pageerror', error => errors.push(error.message));
page.on('request', request => requests.push(new URL(request.url()).pathname));
const count = path => requests.filter(value => value === path).length;
const status = async () => (await context.request.get(`${url}/api/status`)).json();
const pose = async () => (await context.request.get(`${url}/api/scene/state`)).json();
const post = async (path, data = {}) => {
  const response = await context.request.post(`${url}/api/${path}`, { data });
  assert(response.ok(), `${path}: ${await response.text()}`);
};
const waitForStatus = text => page.waitForFunction(
  value => document.querySelector('#view-status').textContent.includes(value), text);
const waitForLidar = expected => page.waitForFunction(async value => {
  const response = await fetch('/api/scene/lidar');
  const sample = await response.json();
  if (value === 'fresh') return sample.fresh && sample.points.length > 0;
  if (value === 'disabled') return !sample.enabled && !sample.fresh && sample.points.length === 0;
  if (value === 'dropout') return sample.enabled && sample.fresh && sample.points.length === 0;
  if (value === 'paused') return sample.mode === 'paused' && !sample.fresh && sample.points.length === 0;
  return false;
}, expected);

async function checkLidarUI() {
  console.log('Checking lidar overlay and effective sensor faults');
  assert.equal(await page.locator('#show-lidar').getAttribute('aria-pressed'), 'true');
  await waitForLidar('fresh');
  const before = (await status()).sensor_settings;
  await page.click('#show-lidar');
  assert.equal(await page.locator('#show-lidar').getAttribute('aria-pressed'), 'false');
  await page.waitForTimeout(250);
  const disabledRequests = count('/api/scene/lidar');
  await page.waitForTimeout(450);
  assert.equal(count('/api/scene/lidar'), disabledRequests, 'Hidden lidar overlay must stop fetching');
  assert.deepEqual((await status()).sensor_settings, before, 'Display toggle must not disable the robot lidar');
  await page.click('#show-lidar');
  await page.waitForResponse(response => new URL(response.url()).pathname === '/api/scene/lidar');
  await post('sensors', { lidar_enabled: false });
  await waitForLidar('disabled');
  await post('sensors', { lidar_enabled: true, lidar_dropout: 1 });
  await waitForLidar('dropout');
  await post('sensors', { lidar_dropout: 0 });
  await waitForLidar('fresh');
}

async function checkFollowMath() {
  // Import the real viewer into a test-only page, with deterministic physics
  // poses. No production globals or access to the live viewer are required.
  const harness = await context.newPage();
  await harness.route(`${url}/__viewer_test__`, route => route.fulfill({
    contentType: 'text/html', body: '<canvas style="width:800px;height:450px"></canvas><span></span>',
  }));
  await harness.goto(`${url}/__viewer_test__`);
  try {
    await harness.evaluate(async () => {
      const { SandboxViewer } = await import('/viewer.js');
      class TestViewer extends SandboxViewer { poll() {} }
      const viewer = new TestViewer(document.querySelector('canvas'), document.querySelector('span'));
      viewer.renderer.setAnimationLoop(null);
      viewer.setLidarEnabled(false);
      viewer.loadScene({ version: 1, id: 'test-scene', robot_body: 1,
        bodies: [{ id: 0, name: 'world' }, { id: 1, name: 'robot' }], meshes: [], geoms: [] });
      const check = (condition, message) => { if (!condition) throw Error(message); };
      const near = (a, b, message) => check(a.length === b.length &&
        a.every((value, index) => Math.abs(value - b[index]) < 1e-7), `${message}: ${a} != ${b}`);
      const add = (a, b) => a.map((value, index) => value + b[index]);
      const subtract = (a, b) => a.map((value, index) => value - b[index]);
      const capture = () => ({ camera: viewer.camera.position.toArray(),
        target: viewer.controls.target.toArray(), rotation: viewer.camera.quaternion.toArray() });
      let time = 0;
      const pose = (position, { epoch = 1, generation = 0, quaternion = [0, 0, 0, 1] } = {}) => {
        viewer.acceptState({ scene_id: 'test-scene', epoch, generation, time: ++time,
          valid: true, mode: 'moving', positions: [0, 0, 0, ...position],
          quaternions: [0, 0, 0, 1, ...quaternion] });
        viewer.samples.at(-1).received = performance.now() - 100;
        viewer.draw(performance.now());
      };
      try {
        pose([0, 0, 0.3]);
        check(viewer.followRobot, 'Follow robot must be enabled initially');
        const start = capture(), translation = [2, 1, 0.2];
        pose([2, 1, 0.5]);
        near(capture().camera, add(start.camera, translation), 'Follow camera translation');
        near(capture().target, add(start.target, translation), 'Follow target translation');
        const translated = capture();
        pose([2, 1, 0.5], { quaternion: [0, 0, Math.SQRT1_2, Math.SQRT1_2] });
        near(capture().camera, translated.camera, 'Robot rotation must preserve orbit angle');
        near(capture().rotation, translated.rotation, 'Robot rotation must preserve viewing direction');

        viewer.controls.target.x += 0.7;
        viewer.controls.target.y -= 0.4;
        viewer.camera.position.x += 0.7;
        viewer.camera.position.y -= 0.4;
        viewer.controls.update();
        const panned = capture();
        pose([3, 0.5, 0.5]);
        near(capture().target, add(panned.target, [1, -0.5, 0]), 'Follow must retain pan offset');
        near(subtract(capture().camera, capture().target), subtract(panned.camera, panned.target),
          'Follow must preserve orbit angle and zoom');

        viewer.setFollowRobot(false);
        const fixed = capture();
        pose([4, 2, 0.5]);
        near(capture().camera, fixed.camera, 'Free camera must stay fixed');
        near(capture().target, fixed.target, 'Free camera target must stay fixed');
        viewer.setFollowRobot(true);
        near(capture().target, [4, 2, 0.5], 'Enabling follow must center the current robot');
        near(subtract(capture().camera, capture().target), subtract(fixed.camera, fixed.target),
          'Enabling follow must preserve orbit angle and zoom');

        const beforeReset = capture();
        time = -1;
        pose([0, 0, 0.3], { epoch: 2, generation: 1 });
        near(capture().camera, add(beforeReset.camera, [-4, -2, -0.2]),
          'World reset must follow teleport without blending epochs');
        near(capture().target, [0, 0, 0.3], 'World reset must target the new robot position');
        const beforeSwitch = capture();
        viewer.setActive(false);
        viewer.setActive(true);
        viewer.draw(performance.now());
        near(capture().camera, beforeSwitch.camera, 'Switching views must preserve orbit');
        viewer.setFollowRobot(false);
        viewer.resetView();
        check(!viewer.followRobot, 'Reset view must retain free-camera mode');
        const resetFree = capture();
        time = -1;
        pose([1, 2, 0.3], { epoch: 3, generation: 2 });
        near(capture().camera, resetFree.camera, 'World reset in free mode must retain camera position');

        const { LidarView } = await import('/lidar-view.js');
        const overlay = new LidarView(viewer.scene), originalFetch = window.fetch;
        const identity = { scene_id: 'lidar-fixture', epoch: 1, generation: 0, mode: 'moving', valid: true };
        let sample = { ...identity, time: 1, enabled: true, available: true, fresh: true, age_ms: 0,
          origin: [0.2, 0, 0.3], quaternion: [0, 0, 0, 1], points: [1, 2, 0.1, 3, 4, 0.2] };
        let fetches = 0;
        window.fetch = async () => { fetches++; return new Response(JSON.stringify(sample)); };
        const captureLidar = async () => {
          overlay.setStateIdentity(identity);
          await overlay.poll(performance.now());
          overlay.nextPoll = Infinity;
        };
        try {
          await captureLidar();
          check(overlay.group.name === 'lidar-overlay' && overlay.group.visible, 'Fresh lidar must be visible');
          check(overlay.group.userData.pointCount === 2 && overlay.geometry.drawRange.count === 2,
            'Overlay must draw exactly the captured returns');
          near(Array.from(overlay.positions.slice(0, 6)), sample.points, 'Lidar returns stay in world coordinates');
          near(overlay.marker.position.toArray(), sample.origin, 'Lidar marker follows the captured mount');
          overlay.group.traverse(object => check(object.layers.mask === 2, 'Lidar must use its diagnostic layer'));
          check(overlay.update(performance.now()), 'New lidar points must request a browser redraw');
          check(overlay.update(performance.now() + 600), 'Expiring lidar must request a browser redraw');
          check(!overlay.group.visible && overlay.geometry.drawRange.count === 0, 'Stale returns must disappear');

          await captureLidar();
          overlay.setEnabled(false);
          const beforeFetches = fetches;
          overlay.update(performance.now());
          check(!overlay.group.visible && fetches === beforeFetches, 'Hidden overlay must stop fetching and clear points');
          overlay.setEnabled(true);
          sample = { ...sample, enabled: false, fresh: false, points: [], origin: null, quaternion: null };
          await captureLidar();
          check(!overlay.group.visible && overlay.group.userData.status === 'disabled', 'Disabled sensor must clear overlay');
          sample = { ...sample, enabled: true, fresh: true, origin: [0.2, 0, 0.3], quaternion: [0, 0, 0, 1] };
          await captureLidar();
          check(overlay.group.visible && overlay.geometry.drawRange.count === 0,
            'Complete dropout must retain only the real sensor mount, without fabricated returns');

          sample = { ...sample, points: [1, 2, 3] };
          await captureLidar();
          overlay.setStateIdentity({ ...identity, mode: 'paused' });
          check(!overlay.group.visible && overlay.update(performance.now()), 'Pausing must immediately clear and redraw lidar');
          overlay.setStateIdentity(identity);
          let finishOldRequest;
          window.fetch = () => new Promise(resolve => { finishOldRequest = resolve; });
          const pending = overlay.poll(performance.now());
          overlay.setStateIdentity({ ...identity, epoch: 2 });
          finishOldRequest(new Response(JSON.stringify(sample)));
          await pending;
          check(!overlay.group.visible, 'A response from before reset must never restore old lidar points');
          window.fetch = async () => new Response(JSON.stringify(sample));
          await captureLidar();
          overlay.setStateIdentity({ ...identity, valid: false, mode: 'fault' });
          check(!overlay.group.visible && overlay.geometry.drawRange.count === 0, 'Physics faults must immediately clear lidar');
        } finally {
          window.fetch = originalFetch;
          overlay.dispose();
        }
      } finally {
        viewer.resize.disconnect();
        viewer.controls.dispose();
        viewer.renderer.dispose();
        viewer.lidar?.dispose();
      }
    });
  } finally {
    await harness.close();
  }
}
try {
  await checkFollowMath();
  await post('sensors', { camera_enabled: true, lidar_enabled: true, lidar_dropout: 0 });
  await post('reset');
  await page.goto(url);
  await waitForStatus('Live 3D');
  await page.click('#pause');
  await waitForStatus('Paused');
  console.log('Checking camera navigation in a paused world');
  // Inspect a frozen world: pixel changes now prove camera navigation works.
  const canvas = page.locator('#view');
  await page.waitForTimeout(250);
  const initial = await canvas.screenshot();
  console.log('Captured initial frame');
  const bounds = await canvas.boundingBox();
  const x = bounds.x + bounds.width / 2, y = bounds.y + bounds.height / 2;
  const drag = async button => {
    await page.mouse.move(x, y);
    await page.mouse.down({ button });
    await page.mouse.move(x + 130, y + 45, { steps: 12 });
    await page.mouse.up({ button });
    await page.waitForTimeout(600);
  };
  await drag('left');
  console.log('Dragged orbit');
  const orbited = await canvas.screenshot();
  assert(!initial.equals(orbited), 'Orbit must change the rendered view');
  await drag('right');
  console.log('Dragged pan');
  const panned = await canvas.screenshot();
  assert(!orbited.equals(panned), 'Right drag must pan the rendered view');
  await page.mouse.wheel(0, -350);
  await page.waitForTimeout(600);
  assert(!panned.equals(await canvas.screenshot()), 'Scroll must zoom');
  await page.click('#reset-view');
  await page.waitForTimeout(600);
  const reset = await canvas.screenshot();
  await page.waitForTimeout(500);
  assert(reset.equals(await canvas.screenshot()), 'Reset view must clear residual damping');
  assert(!(await status()).armed, 'Camera navigation must not acquire robot control');
  assert.equal(count('/api/scene'), 1, 'Geometry should load once');
  assert(count('/api/scene/state') > 5, 'Viewer must receive live state');
  assert.equal(count('/frame.jpg') + count('/camera.jpg'), 0, 'Sandbox must not request JPEGs');
  assert.equal(await page.locator('#follow-robot').getAttribute('aria-pressed'), 'true');
  await page.click('#follow-robot');
  assert.equal(await page.locator('#follow-robot').getAttribute('aria-pressed'), 'false');
  await page.click('#follow-robot');
  assert.equal(await page.locator('#follow-robot').getAttribute('aria-pressed'), 'true');
  console.log('Checking reconnect and sensor request cancellation');

  await context.setOffline(true);
  await waitForStatus('Connection interrupted');
  await context.setOffline(false);
  await waitForStatus('Paused');

  console.log('Checking actual robot sensor camera');
  await page.click('#resume');
  await waitForStatus('Live 3D');
  await page.click('#camera');
  await waitForStatus('Robot sensor camera');
  assert.deepEqual(await page.locator('#camera-view').evaluate(image =>
    [image.naturalWidth, image.naturalHeight]), [640, 360], 'Robot view must decode the actual sensor image');
  await post('sensors', { camera_enabled: false });
  await page.waitForFunction(() => /disabled|unavailable/i.test(document.querySelector('#view-status').textContent));
  await post('sensors', { camera_enabled: true });
  await waitForStatus('Robot sensor camera');
  await page.click('#observer');
  await waitForStatus('Live 3D');
  await checkLidarUI();
  await page.click('#pause');
  await waitForStatus('Paused');
  await waitForLidar('paused');
  assert.equal(count('/api/scene'), 1, 'Reconnect should reuse the model');

  // A slow sensor request must be cancelled when returning to the sandbox.
  let stalledRequests = 0;
  await page.route('**/camera.jpg', async route => {
    stalledRequests++;
    await new Promise(resolve => setTimeout(resolve, 600));
    await route.fulfill({ status: 503, body: '{}' }).catch(() => {});
  });
  await page.click('#camera');
  await page.waitForTimeout(100);
  await page.click('#observer');
  await waitForStatus('Paused');
  await page.click('#camera');
  await page.waitForTimeout(150);
  assert(stalledRequests >= 2, 'Camera must restart after an aborted request');
  await page.click('#observer');
  await waitForStatus('Paused');
  const cameraRequests = count('/camera.jpg');
  await page.waitForTimeout(900);
  assert.equal(count('/camera.jpg'), cameraRequests, 'Hidden sensor camera must stop fetching');
  await waitForStatus('Paused');
  await page.unroute('**/camera.jpg');

  console.log('Checking world reset and keyboard walking');
  const oldEpoch = (await pose()).epoch;
  await page.click('#reset');
  await page.waitForFunction(epoch => Number(document.querySelector('#view').dataset.epoch) > epoch, oldEpoch);
  await waitForStatus('Live 3D');
  assert.equal(count('/api/scene'), 1, 'World reset should reuse geometry');
  const before = await pose();
  await page.click('#arm');
  await page.waitForFunction(() => document.querySelector('#mode').textContent.includes('controls enabled'));
  await page.keyboard.down('w');
  await page.waitForTimeout(900);
  await page.keyboard.up('w');
  await page.keyboard.press('Space');
  await page.click('#pause');
  await waitForStatus('Paused');
  const after = await pose();
  assert(after.positions.some((value, i) => Math.abs(value - before.positions[i]) > 0.05),
    `Keyboard motion must change physics poses (${count('/api/command')} command requests)`);
  await page.setViewportSize({ width: 390, height: 844 });
  assert(await canvas.isVisible(), '3D viewer must remain visible on a narrow screen');
  assert.deepEqual(errors, [], 'Browser must not have uncaught exceptions');
  console.log(JSON.stringify({ passed: true, checks: ['orbit', 'pan', 'zoom', 'reset view', 'follow and free camera', 'state-only sandbox transport', 'offline recovery', 'sensor cancellation', 'actual sensor camera', 'camera fault recovery', 'lidar display and sensor faults', 'world reset', 'walking', 'responsive canvas'], sceneRequests: count('/api/scene'), stateRequests: count('/api/scene/state') }));
} finally {
  await post('sensors', { camera_enabled: true, lidar_enabled: true, lidar_dropout: 0 }).catch(() => {});
  await post('reset').catch(() => {});
  await browser.close();
}
