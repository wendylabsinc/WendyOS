// Real Console DOM only. No route mocks, injected state, RPC substitute or
// expected-answer file. A dedicated signed-in browser supplies normal identity.
import { mkdir } from 'node:fs/promises';

class ConsoleProbeFailure extends Error {}
let phase = 'browser connection';
async function main() {
  const { chromium } = await import('playwright');
  const [path, output = 'console-observation'] = process.argv.slice(2);
  if (process.env.WENDY_E2E_APPS_BROWSER !== 'true') throw new Error('Browser opt-in required');
  const origin = new URL(process.env.WENDY_E2E_APPS_CONSOLE_URL);
  const cdp = new URL(process.env.WENDY_E2E_APPS_BROWSER_CDP);
  if (origin.protocol !== 'https:' || !origin.hostname.endsWith('.dev.wendy.sh')) throw new Error('Dev Console required');
  if (!['127.0.0.1', 'localhost', '[::1]'].includes(cdp.hostname)) throw new Error('Dedicated local CDP browser required');
  if (!/^\/(apps(?:\/[^/?#]+(?:\/(?:overview|deployments|releases|access|settings))?)?|deployments(?:\/[^/?#]+\/(?:overview|devices|history))?|assets\/[^/?#]+)$/.test(path)) throw new Error('Unsupported public Console page');
  const browser = await chromium.connectOverCDP(cdp.href);
  const context = browser.contexts()[0];
  if (!context) throw new Error('A signed-in dedicated browser context is required');
  const page = await context.newPage();
  function requireConsoleLocation() {
    const current = new URL(page.url());
    if (current.origin !== origin.origin || /\/(?:login|sign-in|signin|authorize)(?:\/|$)/i.test(current.pathname)) {
      // Never print the query/fragment: OAuth redirects may contain private state.
      throw new ConsoleProbeFailure(`Console authentication required: redirected to ${current.origin}${current.pathname}. Sign in to the expected Dev organization in the dedicated CDP browser, then rerun the probe.`);
    }
  }
  try {
    phase = 'Console navigation';
    await page.goto(new URL(path, origin).href, { waitUntil: 'domcontentloaded', timeout: 30000 });
    requireConsoleLocation();
    phase = 'signed-in main landmark';
    const main = page.locator('main');
    try {
      await main.waitFor({ state: 'visible', timeout: 30000 });
    } catch (error) {
      requireConsoleLocation();
      throw error;
    }
    requireConsoleLocation();
    // Wait for the rendered loading state, rather than guessing with fixed sleep.
    phase = 'loaded Console detail';
    await page.waitForFunction(route => {
      const main = document.querySelector('main');
      if (!main) return false;
      if ([...main.querySelectorAll('[aria-live]')].some(e => /^Loading\b/i.test(e.innerText.trim()))) return false;
      // Emit an error page as evidence; Swift refuses it before any assertion.
      if (main.querySelector('[role="alert"]') || /Not yours to see/.test(main.innerText)) return true;
      if (/^\/apps\//.test(route)) return !!main.querySelector('nav[aria-label="Pages of this app"]');
      if (/^\/deployments\//.test(route)) return !!main.querySelector('nav[aria-label="Pages of this deployment"]');
      if (/^\/assets\//.test(route)) return !!main.querySelector('section[aria-labelledby="reported-apps-title"]');
      return !!main.querySelector('table caption') || /No apps yet/.test(main.innerText);
    }, path, { timeout: 30000 });
    phase = 'DOM observation';
    const observation = await main.evaluate(element => ({
      text: element.innerText,
      alerts: [...element.querySelectorAll('[role="alert"]')].map(e => e.innerText),
      loading: [...element.querySelectorAll('[aria-live]')].some(e => /^Loading\b/i.test(e.innerText.trim())),
      assetDetailLoaded: !!element.querySelector('section[aria-labelledby="reported-apps-title"]'),
      appListComplete: /No apps yet/.test(element.innerText) || [...element.querySelectorAll('table caption')].some(e => {
        const caption = e.innerText.trim();
        const count = caption.match(/^(\d+) apps?\b/);
        return count && !/\d+ of \d+/.test(caption) && e.closest('table').querySelectorAll('tbody tr').length === Number(count[1]);
      }),
      appDetailLoaded: !!element.querySelector('nav[aria-label="Pages of this app"]'),
      deploymentDetailLoaded: !!element.querySelector('nav[aria-label="Pages of this deployment"]'),
      appListLoaded: [...element.querySelectorAll('h1')].some(e => e.innerText === 'Apps') &&
        (!![...element.querySelectorAll('table th')].find(e => e.innerText.trim().toLowerCase() === 'latest release') || /No apps yet/.test(element.innerText)),
      facts: [...element.querySelectorAll('section[aria-labelledby] dl dt')].map(term => {
        const section = term.closest('section');
        const dd = term.parentElement.querySelector('dd')?.cloneNode(true);
        dd?.querySelectorAll('small').forEach(e => e.remove());
        return { panel: document.getElementById(section.getAttribute('aria-labelledby'))?.innerText ?? '',
          term: term.innerText, value: dd?.textContent.trim() ?? '' };
      }),
      headings: [...element.querySelectorAll('h1,h2,h3')].map(e => e.innerText),
      rows: [...element.querySelectorAll('tbody tr')].map(row => ({
        cells: [...row.querySelectorAll('td')].map(e => e.innerText),
        links: [...row.querySelectorAll('a[href]')].map(e => e.getAttribute('href')),
        titles: [...row.querySelectorAll('[title]')].map(e => e.getAttribute('title')),
      })),
      links: [...element.querySelectorAll('a[href]')].map(e => e.getAttribute('href')),
    }));
    await mkdir(output, { recursive: true });
    await page.screenshot({ path: `${output}/page.png`, fullPage: true });
    const observedURL = new URL(page.url());
    requireConsoleLocation();
    process.stdout.write(JSON.stringify({ observedAtUTC: new Date().toISOString(), url: observedURL.origin + observedURL.pathname, ...observation }) + '\n');
  } finally {
    await page.close();
    await browser.close(); // Disconnect CDP; leave the pre-existing browser running.
  }

}
main().catch(error => {
  // Playwright diagnostics can contain ANSI/XML-illegal controls and full
  // OAuth redirect URLs. Never serialize the library's message or stack.
  const message = error instanceof ConsoleProbeFailure ? error.message
    : `Console DOM probe failed during ${phase}. Confirm normal sign-in to the expected Dev organization in the dedicated CDP browser, then rerun. Raw browser diagnostics are omitted.`;
  process.stderr.write(message.replace(/[\x00-\x1f\x7f-\x9f]/g, ' ') + '\n');
  process.exitCode = 1;
});
