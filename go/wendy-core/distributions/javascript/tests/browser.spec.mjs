import { test, expect } from '@playwright/test';

test('same package, exports, and conformance suite in the browser', async ({ page }) => {
  await page.goto('http://127.0.0.1:4173/tests/browser.html');
  await expect.poll(() => page.evaluate(() => globalThis.result)).toBe(true);
  expect(await page.evaluate(() => globalThis.failure)).toBeUndefined();
  if (process.env.WENDY_CORE_VERSION) {
    expect(await page.evaluate(() => globalThis.coreVersion)).toBe(process.env.WENDY_CORE_VERSION);
  }
});
