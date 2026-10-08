// Run through TestGatewaySimulatorSceneBrowser so the UI uses a real, authorized
// gateway scene session. All simulator requests in this check are observer GETs.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
const { chromium } = await import(
  process.env.PLAYWRIGHT_MODULE || "playwright"
);
const session = JSON.parse(process.env.WENDY_SCENE_TEST_SESSION);
const profile = process.env.WENDY_SCENE_TEST_PROFILE;
const calls = [];
const paths = [];
let activeReads = 0,
  maxActiveReads = 0;
const stateTimes = [];
const scenePayloadBytes = [];
let resourceReads = 0;
const host = `<!doctype html><style>body{margin:0}iframe{border:0;width:100%;height:1500px}</style><iframe id="panel" src="/panel"></iframe><script>
const frame=document.getElementById('panel');
window.displayRequests=[];window.uiActions=[];let displayMode='inline';
window.addEventListener('message',async event=>{
 if(event.source!==frame.contentWindow)return;
 const m=event.data;if(!m||m.jsonrpc!=='2.0')return;
 let result={};try{
  if(m.method==='ui/initialize')result={protocolVersion:'2026-01-26',hostInfo:{name:'Scene integration host',version:'1'},hostCapabilities:{serverTools:{},serverResources:{},updateModelContext:{},message:{}},hostContext:{theme:'light',displayMode,availableDisplayModes:window.disableFullscreen?['inline']:['inline','fullscreen']}};
  else if(m.method==='ui/notifications/initialized'){const r=await fetch('/call',{method:'POST',body:JSON.stringify({name:'open_devices',arguments:{}})});frame.contentWindow.postMessage({jsonrpc:'2.0',method:'ui/notifications/tool-result',params:await r.json()},location.origin);return;}
  else if(m.method==='tools/call'){window.uiActions.push(m.params.name);if(m.params.name==='simulator_scene_read'&&window.sceneFailure)throw Error(window.sceneFailure);const r=await fetch('/call',{method:'POST',body:JSON.stringify(m.params)});if(!r.ok)throw Error(await r.text());result=await r.json();}
  else if(m.method==='ui/request-display-mode'){window.displayRequests.push(m.params.mode);window.uiActions.push('fullscreen');if(window.rejectFullscreen)throw Error('Fullscreen is unavailable in this host.');displayMode=m.params.mode;result={mode:displayMode};if(displayMode==='fullscreen'){frame.style.height='100vh';frame.style.position='fixed';frame.style.inset='0';}frame.contentWindow.postMessage({jsonrpc:'2.0',method:'ui/notifications/host-context-changed',params:{displayMode}},location.origin);}
  else if(m.method==='resources/read'){await fetch('/resource',{method:'POST'});throw Error('MCP app cannot read resource outside its widget scope.');}
  else if(m.method==='ui/notifications/size-changed'&&displayMode!=='fullscreen')frame.style.height=m.params.height+'px';
  if(m.id!==undefined)frame.contentWindow.postMessage({jsonrpc:'2.0',id:m.id,result},location.origin);
 }catch(e){if(m.id!==undefined)frame.contentWindow.postMessage({jsonrpc:'2.0',id:m.id,error:{code:-32603,message:e.message}},location.origin)}
});</script>`;
const server = createServer(async (request, response) => {
  try {
    if (request.url === "/" && request.method === "GET") {
      response.setHeader("Content-Type", "text/html");
      response.end(host);
      return;
    }
    if (request.url === "/panel" && request.method === "GET") {
      response.setHeader("Content-Type", "text/html");
      response.end(
        await readFile(
          new URL(
            "../../../go/internal/cli/mcp/desktop_app.html",
            import.meta.url,
          ),
        ),
      );
      return;
    }
    if (
      !["/call", "/resource"].includes(request.url) ||
      request.method !== "POST"
    ) {
      response.writeHead(404).end();
      return;
    }
    let body = "";
    for await (const chunk of request) body += chunk;
    if (request.url === "/resource") {
      resourceReads++;
      response
        .writeHead(403)
        .end("MCP app cannot read resource outside its widget scope.");
      return;
    }
    const { name, arguments: args } = JSON.parse(body);
    calls.push(name);
    let structuredContent, metadata;
    if (name === "open_devices" || name === "list_robots")
      structuredContent = {
        robots: [
          {
            id: "physical-device",
            name: "Physical fixture device",
            source: "configured",
          },
          { id: "sim-test-scene", name: "test-scene", source: "simulator" },
        ],
        total_count: 2,
        simulator_count: 1,
        can_manage_simulators: true,
      };
    else if (name === "read_device_settings")
      structuredContent = { values: {} };
    else if (name === "simulator_list")
      structuredContent = {
        simulators: [
          {
            name: "test-scene",
            state: "running",
            device: "vm:test-scene",
            profile,
          },
        ],
      };
    else if (name === "simulator_viewer") {
      assert.equal(args.name, "test-scene");
      assert.equal(args.embedded, true);
      structuredContent = {
        name: "test-scene",
        profile,
        ready: true,
        healthy: true,
        url: session.url,
      };
      metadata = { scene_session: session };
    } else if (
      name === "simulator_scene_read" ||
      name === "simulator_scene_pause" ||
      name === "simulator_scene_close"
    ) {
      assert.equal(args.session_id, session.token);
      const reading = name === "simulator_scene_read";
      if (reading) {
        assert.ok(["geometry", "state"].includes(args.part));
        paths.push({
          path: args.part === "geometry" ? "/api/scene" : "/api/scene/state",
          method: "GET",
        });
        if (args.part === "state") stateTimes.push(Date.now());
        maxActiveReads = Math.max(maxActiveReads, ++activeReads);
      } else paths.push({ path: name === "simulator_scene_pause" ? "/pause" : "/session", method: name === "simulator_scene_pause" ? "POST" : "DELETE" });
      try {
        const result = await fetch(process.env.WENDY_SCENE_TEST_BRIDGE, {
          method: "POST",
          body,
        });
        assert.equal(result.status, 200);
        response.setHeader("Content-Type", "application/json");
        const payload = await result.text();
        if (reading && args.part === "geometry") scenePayloadBytes.push(Buffer.byteLength(payload));
        response.end(payload);
      } finally {
        if (reading) activeReads--;
      }
      return;
    } else throw Error(`Unexpected tool call: ${name}`);
    response.setHeader("Content-Type", "application/json");
    response.end(
      JSON.stringify({
        structuredContent,
        content: [],
        ...(metadata && { _meta: metadata }),
      }),
    );
  } catch (error) {
    response.writeHead(500).end(error.message);
  }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const browser = await chromium.launch({
  headless: true,
  channel: process.env.PLAYWRIGHT_CHANNEL || "chrome",
});
try {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1100 },
  });
  const page = await context.newPage();
  page.setDefaultTimeout(30_000);
  const errors = [],
    directSceneRequests = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("request", (request) => {
    if (request.url().startsWith(session.url))
      directSceneRequests.push({
        path: new URL(request.url()).pathname,
        method: request.method(),
      });
  });
  await context.route(session.url + "/**", (route) =>
    route.abort("blockedbyclient"),
  );
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  const panel = page.frameLocator("#panel");
  await panel
    .locator(".device-card")
    .filter({ hasText: "Physical fixture device" })
    .waitFor();
  assert.equal(
    await panel.locator(".device-card").count(),
    1,
    "Devices must show only physical devices",
  );
  assert.equal(
    await panel
      .locator(".device-nav")
      .getByText("test-scene", { exact: true })
      .count(),
    0,
    "Device sidebar must omit simulators",
  );
  assert.match(
    await panel.locator(".fleet-summary").innerText(),
    /^1 device\b/,
  );
  await panel.getByRole("button", { name: "Simulators", exact: true }).click();
  await panel
    .getByRole("button", { name: "View simulation", exact: true })
    .click();
  await panel
    .locator(".simulator-render-status")
    .filter({ hasText: "Live 3D" })
    .waitFor();
  const canvas = panel.locator("canvas.simulator-viewer-frame");
  assert.deepEqual(await page.evaluate(() => window.displayRequests), ["fullscreen"]);
  const uiActions = await page.evaluate(() => window.uiActions);
  assert.ok(uiActions.indexOf("fullscreen") < uiActions.indexOf("simulator_viewer"), "Fullscreen must be requested directly from the action before waiting for the viewer");
  assert.equal(await panel.locator(".simulator-create").isVisible(), false);
  const canvasBounds = await canvas.boundingBox();
  assert.ok(canvasBounds.y >= 0 && canvasBounds.y + canvasBounds.height <= 1100, "The live scene must be visible without scrolling");
  await page.waitForTimeout(500);
  const first = await canvas.screenshot();
  assert.ok(
    first.length > 10_000,
    "The real scene must render visible geometry",
  );
  if (process.env.WENDY_SCENE_SCREENSHOT)
    await canvas.screenshot({ path: process.env.WENDY_SCENE_SCREENSHOT });
  await panel.getByRole("button", { name: "Zoom in", exact: true }).click();
  await page.waitForTimeout(200);
  assert.ok(
    !first.equals(await canvas.screenshot()),
    "Zoom must change rendered pixels",
  );
  const box = await canvas.boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(
    box.x + box.width / 2 + 100,
    box.y + box.height / 2 + 40,
    { steps: 8 },
  );
  await page.mouse.up();
  await panel.getByRole("checkbox", { name: "Follow robot" }).uncheck();
  await panel
    .getByRole("button", { name: "Reset camera", exact: true })
    .click();
  const countStates = () =>
    paths.filter((r) => r.path === "/api/scene/state").length;
  assert.ok(countStates() > 2, "Live poses must arrive through tool calls");
  assert.ok(Number(await canvas.getAttribute("data-pose-sequence")) > countStates() * 2, "Batches must deliver intermediate poses between host polls");
  assert.deepEqual(
    directSceneRequests,
    [],
    "The component must not fetch localhost directly",
  );
  assert.equal(
    paths.filter((r) => r.path === "/api/scene").length,
    1,
    "Geometry must load once",
  );
  assert.equal(
    calls.filter((name) => name === "simulator_viewer").length,
    1,
    "Frames must reuse the viewer session",
  );
  assert.equal(
    resourceReads,
    0,
    "Widget must not read resources outside its scope",
  );
  assert.equal(maxActiveReads, 1, "Only one scene request may be active");
  for (let i = 1; i < stateTimes.length; i++)
    assert.ok(
      stateTimes[i] - stateTimes[i - 1] >= 245,
      "Pose reads must stay at or below 4 Hz",
    );
  await context.setOffline(true);
  await panel
    .locator(".simulator-render-status")
    .filter({ hasText: "Connection interrupted" })
    .waitFor();
  await context.setOffline(false);
  await panel
    .locator(".simulator-render-status")
    .filter({ hasText: "Live 3D" })
    .waitFor();
  // Deterministically exercise document visibility without navigating the app.
  const frame = page.frames().find((frame) => frame.url().endsWith("/panel"));
  await frame.evaluate(() => {
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => true,
    });
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await page.waitForTimeout(150);
  const hiddenCount = countStates();
  await page.waitForTimeout(350);
  assert.equal(countStates(), hiddenCount, "Hidden views must stop polling");
  const resumed = page.waitForResponse((response) => {
    const request = response.request();
    return (
      request.url().endsWith("/call") &&
      request.postDataJSON().name === "simulator_scene_read" &&
      request.postDataJSON().arguments.part === "state"
    );
  });
  await frame.evaluate(() => {
    delete document.hidden;
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await resumed;
  assert.ok(countStates() > hiddenCount, "Visible views must resume polling");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForTimeout(150);
  const narrowBounds = await canvas.boundingBox();
  assert.ok(
    narrowBounds.width <= 390,
    "Canvas must fit the narrow panel",
  );
  assert.ok(narrowBounds.y >= 0 && narrowBounds.y + narrowBounds.height <= 844, "Narrow scene must remain visible without scrolling");
  const controlsBounds = await panel.locator(".simulator-view-controls").boundingBox();
  assert.ok(controlsBounds.y >= 0 && controlsBounds.y + controlsBounds.height <= 844, "Camera controls must remain visible without scrolling");
  await canvas.evaluate((element) =>
    element.dispatchEvent(new Event("webglcontextlost", { cancelable: true })),
  );
  await panel
    .getByRole("alert")
    .filter({ hasText: "graphics were interrupted" })
    .waitFor();
  assert.equal(
    await panel
      .getByRole("button", { name: "Zoom in", exact: true })
      .isDisabled(),
    true,
  );
  await panel.getByRole("button", { name: "Back to simulators", exact: true }).click();
  await page.waitForTimeout(300);
  assert.equal(await canvas.count(), 0, "Hide must unmount the renderer");
  assert.equal(await panel.locator(".simulator-create").isVisible(), true, "Back must restore simulator management");
  assert.ok(
    paths.some((r) => r.path === "/session" && r.method === "DELETE"),
    "Hide must close the gateway session",
  );
  const hidden = countStates();
  await page.waitForTimeout(250);
  assert.equal(countStates(), hidden, "Disposed viewer must stop fetching");
  assert.ok(
    paths.every((r) =>
      ["/api/scene", "/api/scene/state", "/pause", "/session"].includes(r.path),
    ),
    "No control endpoint may be requested",
  );
  assert.deepEqual(errors, [], "The browser must have no uncaught errors");
  const failureContext = await browser.newContext();
  const failurePage = await failureContext.newPage();
  await failurePage.goto(`http://127.0.0.1:${server.address().port}`);
  await failurePage.evaluate(() => {
    window.rejectFullscreen = true;
    window.sceneFailure =
      "MCP app cannot read resource outside its widget scope.";
  });
  const failed = failurePage.frameLocator("#panel");
  await failed.getByRole("button", { name: "Simulators", exact: true }).click();
  await failed
    .getByRole("button", { name: "View simulation", exact: true })
    .click();
  await failed.locator('[data-view-focused="true"]').waitFor();
  assert.deepEqual(await failurePage.evaluate(() => window.displayRequests), ["fullscreen"]);
  assert.equal(await failed.locator(".simulator-create").isVisible(), false, "Declining fullscreen must still focus the inline viewer");
  await failed
    .getByRole("alert")
    .filter({ hasText: "scene could not load" })
    .waitFor();
  await failed.getByText("Error details", { exact: true }).click();
  await failed
    .locator("details p")
    .filter({
      hasText: "MCP app cannot read resource outside its widget scope.",
    })
    .waitFor();
  assert.equal(
    await failed.locator(".simulator-render-status").innerText(),
    "Scene unavailable.",
  );
  assert.equal(
    await failed
      .getByRole("button", { name: "Zoom in", exact: true })
      .isDisabled(),
    true,
  );
  await failureContext.close();
  const noGPU = await browser.newContext();
  await noGPU.addInitScript(() => {
    window.disableFullscreen = true;
    const original = HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext = function (type, ...args) {
      return type.startsWith("webgl")
        ? null
        : original.call(this, type, ...args);
    };
  });
  const fallbackPage = await noGPU.newPage();
  await fallbackPage.goto(`http://127.0.0.1:${server.address().port}`);
  const fallback = fallbackPage.frameLocator("#panel");
  await fallback
    .getByRole("button", { name: "Simulators", exact: true })
    .click();
  await fallback
    .getByRole("button", { name: "View simulation", exact: true })
    .click();
  await fallback
    .getByRole("alert")
    .filter({ hasText: "3D graphics are unavailable" })
    .waitFor();
  assert.equal(
    await fallback
      .getByRole("button", { name: "Open in browser", exact: false })
      .isEnabled(),
    true,
  );
  assert.deepEqual(await fallbackPage.evaluate(() => window.displayRequests), [], "Do not request a display mode the host explicitly omits");
  await noGPU.close();
  console.log(
    JSON.stringify({
      passed: true,
      profile,
      sceneRequests: 1,
      scenePayloadBytes,
      poseRequests: countStates(),
      checks: [
        "real scene",
        "simulators excluded from Devices",
        "immediate fullscreen and focused view",
        "fullscreen refusal and unsupported host fallback",
        "return to simulator management",
        "zoom",
        "orbit",
        "camera follow",
        "offline recovery",
        "visibility",
        "narrow layout",
        "context loss",
        "no-WebGL fallback",
        "session cleanup",
        "app-only tool reads",
        "widget scope rejection",
        "loading failure details",
        "single active request",
        "4 Hz pose limit",
        "no robot control",
      ],
    }),
  );
} finally {
  await browser.close();
  server.closeAllConnections();
  await new Promise((resolve) => server.close(resolve));
}
