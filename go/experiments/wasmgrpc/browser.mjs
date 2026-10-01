// node browser.mjs [server URL] [path/to/playwright/index.mjs]
import assert from 'node:assert/strict';
const { chromium } = await import(process.argv[3] || process.env.PLAYWRIGHT_MODULE || 'playwright');
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage();
  const errors = [];
  let sockets = 0, binaryFrames = 0;
  page.on('websocket', socket => {
    sockets++;
    socket.on('framesent', ({ payload }) => {
      assert(Buffer.isBuffer(payload), 'Tunnel must send binary WebSocket messages');
      binaryFrames++;
    });
  });
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(process.argv[2] || 'http://127.0.0.1:8787');
  await page.waitForFunction(() => window.wasmResult, null, { timeout: 60000 });
  const result = await page.evaluate(() => window.wasmResult);
  console.log(JSON.stringify(result, null, 2));
  assert.deepEqual(errors, []);
  assert.equal(result.ok, true, result.error);
  assert.equal(result.checks.length, 5);
  assert.match(result.checks[3], /server certificate belongs to org 7, expected org 8/);
  assert.match(result.checks[4], /certificate required/i);
  assert(sockets >= 4, 'Expected initial, reconnect, and two negative-case WebSockets');
  assert(binaryFrames > 0, 'Expected binary tunnel traffic');
} finally {
  await browser.close();
}
