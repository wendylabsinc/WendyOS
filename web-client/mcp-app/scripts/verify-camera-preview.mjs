// Exercise the real camera component with fixture frames. No camera is activated.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { build } from "esbuild";
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || "playwright");
const bridge = `
export const app={};
export const toolErrorMessage=e=>e.message;
window.calls=[];window.reads=0;let previewReads=0;
export async function call(name,args,options){
 window.calls.push({name,args,background:options?.priority==='background'});
 if(name==='start_camera_preview'){previewReads=0;return {structuredContent:{preview_id:'fixture'}};}
 if(name==='stop_camera_preview')return {};
 if(name!=='read_camera_preview')throw Error('Unexpected '+name);
 if(!options?.signal||options.priority!=='background')throw Error('Unbounded foreground camera read');
 window.reads++;previewReads++;
 if(previewReads===1)return {structuredContent:{sequence:1,width:1,height:1},_meta:{frame:{mimeType:'image/png',data:'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aNc8AAAAASUVORK5CYII='}}};
 if(args.after_sequence!==1)throw Error('Missing last frame sequence');
 if(previewReads===2)return {structuredContent:{sequence:1}};
 return new Promise((resolve,reject)=>options.signal.addEventListener('abort',()=>{window.readAborted=true;reject(options.signal.reason)}, {once:true}));
}`;
const compiled = await build({
  stdin: {
    contents: `import React from 'react';import {createRoot} from 'react-dom/client';import {CameraPanel} from './src/camera';createRoot(document.getElementById('root')).render(<CameraPanel robot="fixture" cameras={[{id:0,name:'Fixture camera'}]}/>);`,
    loader: "tsx", resolveDir: new URL("..", import.meta.url).pathname,
  },
  bundle: true, write: false, format: "iife", jsx: "automatic",
  define: { "process.env.NODE_ENV": '"production"' },
  plugins: [{ name: "fixture-bridge", setup(builder) {
    builder.onResolve({ filter: /^\.\/bridge$/ }, () => ({ path: "bridge", namespace: "fixture" }));
    builder.onLoad({ filter: /.*/, namespace: "fixture" }, () => ({ contents: bridge, loader: "js" }));
  } }],
});
const server = createServer((request, response) => {
  response.setHeader("Content-Type", "text/html");
  response.end(`<div id="root"></div><script>${compiled.outputFiles[0].text.replaceAll("</script", "<\\/script")}</script>`);
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const browser = await chromium.launch({ headless: true, channel: process.env.PLAYWRIGHT_CHANNEL || "chrome" });
try {
  const page = await browser.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  await page.getByRole("button", { name: "Start live preview" }).click();
  await page.waitForFunction(() => window.reads >= 3);
  assert.equal(await page.locator("img.snapshot").count(), 1, "An unchanged response must retain the last image");
  await page.getByRole("button", { name: "Stop preview" }).click();
  await page.waitForFunction(() => window.readAborted && window.calls.some((c) => c.name === "stop_camera_preview"));
  assert.equal(await page.getByRole("alert").count(), 0, "Cancellation must not show an error");
  await page.waitForTimeout(350);
  assert.equal(await page.evaluate(() => window.reads), 3, "Stop must prevent future polls");
  await page.getByRole("button", { name: "Start live preview" }).click();
  await page.waitForFunction(() => window.reads === 6);
  await page.evaluate(() => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await page.waitForFunction(() => window.calls.filter((c) => c.name === "stop_camera_preview").length === 2);
  assert.equal(await page.getByRole("alert").count(), 0);
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ passed: true, checks: ["background reads", "sequence hints", "unchanged frame retained", "stop cancellation", "visibility cleanup"] }));
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
