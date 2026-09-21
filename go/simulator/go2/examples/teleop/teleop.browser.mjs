// Run only against a disposable teleop server with no simulator grant:
// node teleop.browser.mjs http://127.0.0.1:8903 [path/to/playwright/index.mjs]
import assert from 'node:assert/strict';
const { chromium } = await import(process.argv[3] || process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.argv[2] || 'http://127.0.0.1:8903';
assert(['localhost', '127.0.0.1', '[::1]'].includes(new URL(url).hostname), 'Use a disposable local test server');
const browser = await chromium.launch({ headless: true, channel: 'chromium' });
const context = await browser.newContext({ viewport: { width: 1280, height: 1000 } });
const page = await context.newPage();
const errors = [];
page.on('pageerror', error => errors.push(error.message));
const status = async () => (await context.request.get(`${url}/api/status`)).json();
async function waitStatus(check) {
  for (let attempt = 0; attempt < 50; attempt++) {
    const value = await status();
    if (check(value)) return value;
    await page.waitForTimeout(20);
  }
  assert.fail(`Status did not match: ${JSON.stringify(await status())}`);
}
async function enable() {
  await page.click('#enable');
  await page.waitForFunction(() => !document.querySelector('[data-key=w]').disabled);
}
try {
  await page.goto(url);
  assert.equal(await page.locator('[data-key=w]').isDisabled(), true);
  await enable();
  await page.keyboard.down('w');
  await waitStatus(value => value.velocity[0] > 0);
  await page.keyboard.up('w');
  await waitStatus(value => value.velocity.every(component => component === 0));

  const button = await page.locator('[data-key=q]').boundingBox();
  await page.mouse.move(button.x + button.width / 2, button.y + button.height / 2);
  await page.mouse.down();
  await waitStatus(value => value.velocity[2] > 0);
  await page.mouse.up();
  await waitStatus(value => value.velocity.every(component => component === 0));

  await page.keyboard.down('w');
  await waitStatus(value => value.velocity[0] > 0);
  // Dispatch blur directly to exercise the browser lifecycle handler
  // deterministically in a headless test runner.
  await page.evaluate(() => window.dispatchEvent(new Event('blur')));
  await waitStatus(value => !value.enabled && value.velocity.every(component => component === 0));
  await page.keyboard.up('w');
  assert.equal(await page.locator('[data-key=w]').isDisabled(), true);

  await enable();
  await page.keyboard.down('d');
  await waitStatus(value => value.velocity[1] < 0);
  await page.reload();
  await waitStatus(value => !value.enabled);
  assert.equal(await page.locator('[data-key=d]').isDisabled(), true);
  await page.keyboard.up('d');

  await enable();
  await page.keyboard.down('w');
  await waitStatus(value => value.velocity[0] > 0);
  await page.route('**/api/drive', route => route.abort());
  await waitStatus(value => !value.enabled && value.velocity.every(component => component === 0));
  await page.keyboard.up('w');
  await page.unroute('**/api/drive');

  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: '/tmp/go2-teleop-mobile.png', fullPage: true });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  await page.setViewportSize({ width: 1280, height: 1000 });
  await page.screenshot({ path: '/tmp/go2-teleop-desktop.png', fullPage: true });
  assert.deepEqual(errors, []);
  console.log('Teleop browser checks passed: keyboard, pointer, focus loss, reload, disconnect, responsive layout, no JS errors.');
} finally {
  await page.close();
  await context.close();
  await browser.close();
}
