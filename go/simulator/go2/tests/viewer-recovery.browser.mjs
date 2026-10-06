// No simulator needed. Exercise both real viewers with deterministic HTTP
// responses, including graphics loss and intermittent camera failures.
// PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs node tests/viewer-recovery.browser.mjs
// BROWSER_CHANNEL=chrome also runs against the installed Google Chrome.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({ headless: true, channel: process.env.BROWSER_CHANNEL || 'chromium' });
const origin = 'http://viewer-recovery.test';
const scene = { version: 1, id: 'fixture', robot_body: 1, meshes: [],
  bodies: [{ id: 0, name: 'world' }, { id: 1, name: 'robot' }],
  geoms: [{ body: 1, type: 'box', size: [.2, .2, .3], rgba: [.8, .5, .2, 1],
    position: [0, 0, 0], quaternion: [0, 0, 0, 1] }] };
const state = { scene_id: 'fixture', epoch: 1, generation: 0, time: 1,
  valid: true, mode: 'paused', positions: [0, 0, 0, 0, 0, .3],
  quaternions: [0, 0, 0, 1, 0, 0, 0, 1] };

async function setup(robot, mode = 'normal') {
  const page = await browser.newPage({ viewport: { width: 1000, height: 850 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const root = new URL(`../../${robot}/${robot}_sim/`, import.meta.url);
  const modules = [], posts = [];
  let activeModules = 0, maxModules = 0, cameraMode = 'good', cameraCount = 0;
  // A real JPEG, independent of the server's renderer or a fixture file.
  const jpeg = await page.evaluate(() => {
    const canvas = document.createElement('canvas');
    canvas.width = 64; canvas.height = 36;
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = '#eeaa44'; ctx.fillRect(0, 0, 64, 36);
    return canvas.toDataURL('image/jpeg').split(',')[1];
  });
  if (mode === 'no-webgl' || mode === 'fallback') await page.addInitScript(mode => {
    const original = HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext = function(type, attributes) {
      if (type === 'webgl2' && (mode === 'no-webgl' || attributes?.antialias)) return null;
      return original.call(this, type, attributes);
    };
  }, mode);
  await page.route(`${origin}/**`, async route => {
    const request = route.request(), path = new URL(request.url()).pathname;
    if (request.method() === 'POST') posts.push(path);
    if (path === '/') return route.fulfill({ contentType: 'text/html', body: await readFile(new URL('index.html', root)) });
    if (path.endsWith('.js')) {
      modules.push(path); activeModules++; maxModules = Math.max(maxModules, activeModules);
      try {
        // Make simultaneous imports overlap so the test catches bursts.
        await new Promise(resolve => setTimeout(resolve, 30));
        if (mode === 'failed-import' && path.endsWith('OrbitControls.js')) return await route.abort('connectionreset');
        return await route.fulfill({ contentType: 'text/javascript', body: await readFile(new URL(path.slice(1), root)) });
      } finally { activeModules--; }
    }
    if (path === '/camera.jpg') {
      cameraCount++;
      if (cameraMode === 'unavailable') return route.fulfill({ status: 503, json: { error: 'waiting for first frame' } });
      if (cameraMode === 'invalid') return route.fulfill({ contentType: 'image/jpeg', body: 'incomplete JPEG' });
      if (cameraMode === 'slow') await new Promise(resolve => setTimeout(resolve, 600));
      return route.fulfill({ contentType: 'image/jpeg', body: Buffer.from(jpeg, 'base64') }).catch(() => {});
    }
    if (path === '/api/status') return route.fulfill({ json: { armed: false, mode: 'paused',
      metrics: { real_time_factor: 1, camera_frames: 1, wall_seconds: 1 },
      sensor_settings: { camera_enabled: true, lidar_enabled: false, lidar_dropout: 0 } } });
    if (path === '/api/scene') return route.fulfill({ json: scene });
    if (path === '/api/scene/state') return route.fulfill({ json: state });
    if (path === '/api/scene/lidar') return route.fulfill({ json: { enabled: false, fresh: false, points: [] } });
    return route.fulfill({ status: 404 });
  });
  await page.goto(origin);
  return { page, errors, modules, posts, maxModules: () => maxModules,
    cameraCount: () => cameraCount, cameraMode: value => { cameraMode = value; } };
}

try {
  for (const robot of ['g1', 'go2']) {
    const test = await setup(robot, 'fallback');
    const { page } = test;
    const status = page.locator('#view-status');
    await page.waitForFunction(() => document.querySelector('#view-status').textContent.includes('Paused'));
    assert.equal(test.maxModules(), 1, `${robot}: module downloads must be sequential`);
    assert.deepEqual(test.modules.slice(0, 3), ['/vendor/three.core.js', '/vendor/three.module.js', '/vendor/OrbitControls.js']);
    assert(!(await page.locator('#pause').isVisible()), 'Paused scenes show Resume instead of Pause');
    assert(await page.locator('#resume').isVisible());
    assert(!(await page.locator('#source').isVisible()), 'Advanced app controls start collapsed');
    await page.screenshot({ path: join(tmpdir(), `${robot}-ui-desktop.png`), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'The mobile UI must fit without horizontal scrolling');
    await page.waitForTimeout(150);
    await page.screenshot({ path: join(tmpdir(), `${robot}-ui-mobile.png`) });
    await page.locator('details').last().scrollIntoViewIfNeeded();
    await page.screenshot({ path: join(tmpdir(), `${robot}-ui-mobile-settings.png`) });
    await page.setViewportSize({ width: 1000, height: 850 });
    await page.evaluate(() => scrollTo(0, 0));

    // Use a real GPU context loss, not just synthetic DOM events. Three.js
    // rebuilds its resources on restoration; the viewer must resume polling.
    await page.locator('#view').evaluate(canvas => {
      window.loss = canvas.getContext('webgl2').getExtension('WEBGL_lose_context');
      if (!window.loss) throw Error('WEBGL_lose_context is required');
      window.loss.loseContext();
    });
    await page.waitForFunction(() => document.querySelector('#view-status').textContent.includes('waiting for recovery'));
    assert(await page.locator('#retry-viewer').isVisible(), 'A persistent graphics loss must offer retry');
    await page.waitForTimeout(150);
    await page.evaluate(() => window.loss.restoreContext());
    await page.waitForFunction(() => document.querySelector('#view-status').textContent.includes('Paused'));
    assert(!(await page.locator('#retry-viewer').isVisible()), 'Successful recovery must hide retry');
    const pixels = await page.locator('#view').screenshot();
    assert(pixels.length > 2000, `${robot}: the restored scene must render`);

    await page.click('#camera');
    await page.waitForFunction(() => document.querySelector('#view-status').textContent === 'Robot sensor camera');
    const goodURL = await page.locator('#camera-view').getAttribute('src');
    test.cameraMode('invalid');
    await page.waitForTimeout(350);
    assert.equal(await status.textContent(), 'Robot sensor camera', `${robot}: a transient failure must not flicker the label`);
    assert.equal(await page.locator('#camera-view').getAttribute('src'), goodURL, `${robot}: bad JPEGs must preserve the last frame`);
    assert.deepEqual(await page.locator('#camera-view').evaluate(img => [img.naturalWidth, img.naturalHeight]), [64, 36]);
    test.cameraMode('unavailable');
    await page.waitForFunction(() => document.querySelector('#view-status').textContent.includes('waiting for first frame'));
    test.cameraMode('good');
    await page.waitForFunction(() => document.querySelector('#view-status').textContent === 'Robot sensor camera');
    test.cameraMode('slow');
    await page.waitForTimeout(120);
    await page.click('#observer');
    await page.waitForTimeout(750);
    assert(!/Robot sensor camera|first frame/.test(await status.textContent()), `${robot}: late camera responses must not overwrite 3D status`);
    const count = test.cameraCount();
    await page.waitForTimeout(300);
    assert.equal(test.cameraCount(), count, `${robot}: switching views must stop camera polling`);
    assert.deepEqual(test.posts, [], 'Rendering recovery must not change robot controls');
    assert.deepEqual(test.errors, [], `${robot}: no uncaught browser errors`);
    await page.close();

    for (const mode of ['failed-import', 'no-webgl']) {
      const failed = await setup(robot, mode);
      await failed.page.locator('#retry-viewer').waitFor({ state: 'visible' });
      const message = await failed.page.locator('#view-status').textContent();
      assert.match(message, mode === 'failed-import' ? /files could not load/ : /graphics could not start/);
      await failed.page.click('#camera');
      await failed.page.waitForFunction(() => document.querySelector('#view-status').textContent === 'Robot sensor camera');
      await failed.page.click('#observer');
      assert.equal(await failed.page.locator('#view-status').textContent(), message, 'Switching modes must preserve startup diagnostics');
      assert(await failed.page.locator('#retry-viewer').isVisible());
      assert.deepEqual(failed.errors, []);
      await failed.page.close();
    }
    console.log(`${robot}: sequential startup, graphics fallback/recovery, camera recovery and diagnostics passed`);
  }
} finally { await browser.close(); }
