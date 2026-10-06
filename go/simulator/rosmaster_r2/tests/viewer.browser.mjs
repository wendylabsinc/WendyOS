// Run only against a disposable local simulator. This test drives and resets it.
import assert from 'node:assert/strict';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
const {chromium} = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.argv[2] || 'http://127.0.0.1:8895';
assert(['localhost','127.0.0.1'].includes(new URL(url).hostname));
const browser = await chromium.launch({headless:true, executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
try {
  const page = await browser.newPage({viewport:{width:1440,height:1000}});
  const errors=[];
  page.on('pageerror',error=>errors.push(error.message));
  page.on('console',message=>{ if(message.type()==='error') errors.push(message.text()); });
  page.on('response',response=>{ if(response.status()>=400 && new URL(response.url()).pathname!=='/favicon.ico') errors.push(`${response.status()} ${response.url()}`); });
  // The shipped renderer must work without a CDN, images or remote fonts.
  await page.route('**/*',route=>{
    if(new URL(route.request().url()).origin!==new URL(url).origin) {
      errors.push(`External asset: ${route.request().url()}`); return route.abort();
    }
    return route.continue();
  });
  const status=async()=>(await page.request.get(`${url}/api/status`)).json();
  await page.goto(url,{waitUntil:'domcontentloaded'});
  await page.waitForFunction(()=>document.querySelector('#connection').textContent==='Simulator connected');
  await page.waitForFunction(()=>[...document.querySelectorAll('img')].every(image=>image.naturalWidth===320));
  await page.click('#reset');
  await page.waitForTimeout(200);
  await page.click('#enable');
  await page.waitForFunction(()=>document.querySelector('#enable').textContent==='Keyboard controls enabled');
  await page.keyboard.down('w');
  await page.waitForTimeout(700);
  await page.keyboard.down('a');
  await page.waitForTimeout(600);
  await page.keyboard.up('a');
  await page.keyboard.up('w');
  const moving=await status();
  assert(moving.state.x>.15,`forward command moved the car: ${JSON.stringify(moving)} / ${await page.locator('#notice').textContent()}`);
  assert(moving.state.yaw>.1,'front steering curved the path');
  await page.keyboard.press('Space');
  await page.waitForTimeout(100);
  const stopped=await status();
  assert.equal(stopped.state.speed,0);
  assert.equal(stopped.control_mode,'none');
  await page.click('#pause');
  await page.waitForTimeout(150);
  assert(await page.locator('#enable').isDisabled());
  const frozen=(await status()).state.elapsed;
  await page.waitForTimeout(200);
  assert.equal((await status()).state.elapsed,frozen);
  await page.click('#pause');
  await page.click('#reset');
  await page.waitForTimeout(200);
  assert.equal((await status()).state.x,0);
  for (const view of ['top','driver','orbit']) {
    await page.click(`#${view}`); await page.waitForTimeout(200);
    await page.screenshot({path:join(tmpdir(),`rosmaster-r2-${view}.png`)});
  }
  await page.locator('summary').filter({hasText:'Environment'}).click();
  await page.click('#lidar'); await page.click('#lidar');
  await page.click('#follow'); await page.click('#follow');
  await page.click('#enable');
  await page.waitForFunction(()=>document.querySelector('#enable').textContent==='Keyboard controls enabled');
  await page.keyboard.down('ArrowDown');
  await page.waitForTimeout(500);
  await page.keyboard.up('ArrowDown');
  assert((await status()).state.x<-.04,'reverse arrow key moved the car');
  await page.evaluate(()=>window.dispatchEvent(new Event('blur')));
  await page.waitForTimeout(400);
  assert.equal((await status()).state.speed,0,'losing focus stops the car');
  await page.click('#reset'); await page.waitForTimeout(150);
  await page.screenshot({path:join(tmpdir(),'rosmaster-r2-desktop.png')});
  await page.setViewportSize({width:390,height:844});
  await page.waitForTimeout(150);
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile layout fits');
  await page.click('#enable');
  await page.waitForFunction(()=>document.querySelector('#enable').textContent==='Keyboard controls enabled');
  await page.locator('[data-key="w"]').scrollIntoViewIfNeeded();
  const pad=await page.locator('[data-key="w"]').boundingBox();
  await page.mouse.move(pad.x+pad.width/2,pad.y+pad.height/2);
  await page.mouse.down();
  await page.waitForTimeout(400);
  await page.mouse.up();
  assert((await status()).state.x>.02,'pointer controls moved the car');
  await page.click('#stop');
  await page.waitForFunction(()=>document.querySelector('#owner').textContent==='None' && document.querySelector('#speed').textContent==='0.00');
  await page.screenshot({path:join(tmpdir(),'rosmaster-r2-mobile.png'),fullPage:true});
  assert.deepEqual(errors,[]);
  const failedPage = await browser.newPage();
  await failedPage.route('**/vendor/OrbitControls.js', route => route.abort('connectionreset'));
  await failedPage.goto(url, {waitUntil:'domcontentloaded'});
  await failedPage.waitForFunction(() => !document.querySelector('#startup-error').hidden);
  assert.equal(await failedPage.locator('#connection').textContent(), 'Viewer unavailable');
  assert(await failedPage.locator('#reload-viewer').isVisible(), 'failed imports offer recovery');
  await failedPage.close();
  console.log('Browser driving, steering, stop, pause, reset, reverse, focus loss and responsive layout passed.');
} finally { await browser.close(); }
