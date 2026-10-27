import { defineConfig } from '@playwright/test';
export default defineConfig({
  testDir: './tests',
  testMatch: 'browser.spec.mjs',
  webServer: { command: 'node tests/server.mjs', url: 'http://127.0.0.1:4173/tests/browser.html' },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
});
