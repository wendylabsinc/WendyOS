import {
  app,
  BrowserWindow,
  dialog,
  ipcMain,
  net,
  protocol,
  session,
} from "electron";
import { createRequire } from "node:module";
import { join, resolve, sep, delimiter } from "node:path";
import { homedir } from "node:os";
import { pathToFileURL } from "node:url";
import { setTimeout as delay } from "node:timers/promises";
import { APP_URL, trustedSender } from "./policy.mjs";
import { DesktopService } from "./service.mjs";
import { Sessions } from "./sessions.mjs";

const require = createRequire(import.meta.url);
const pty = require("node-pty");
protocol.registerSchemesAsPrivileged([
  {
    scheme: "wendy",
    privileges: { standard: true, secure: true, supportFetchAPI: true },
  },
]);
app.setName("Wendy Desktop");
let window;
let sessions;
let service;
let quitting = false;
const devURL = !app.isPackaged ? process.env.WENDY_DESKTOP_DEV_URL : undefined;
if (devURL && !/^http:\/\/127\.0\.0\.1:\d+\/$/.test(devURL))
  throw new Error("Desktop development must bind to 127.0.0.1.");

if (!app.requestSingleInstanceLock()) app.quit();
else {
  app.on("second-instance", () => {
    window?.restore();
    window?.focus();
  });
  app
    .whenReady()
    .then(start)
    .catch((error) => {
      if (process.env.WENDY_DESKTOP_SMOKE === "1") {
        console.error(error);
        sessions?.close();
        app.exit(1);
        return;
      }
      dialog.showErrorBox(
        "Wendy Desktop could not start",
        error.stack || error.message,
      );
      app.quit();
    });
}

async function start() {
  const root = app.getAppPath();
  const renderer = resolve(root, "dist-desktop");
  protocol.handle("wendy", async (request) => {
    const url = new URL(request.url);
    if (url.hostname !== "app")
      return new Response("Not found", { status: 404 });
    const path = resolve(renderer, "." + decodeURIComponent(url.pathname));
    if (!path.startsWith(renderer + sep))
      return new Response("Forbidden", { status: 403 });
    const response = await net.fetch(pathToFileURL(path).href);
    const headers = new Headers(response.headers);
    headers.set("Cache-Control", "no-store");
    return new Response(response.body, { status: response.status, headers });
  });
  session.defaultSession.setPermissionRequestHandler(
    (_contents, _permission, callback) => callback(false),
  );
  session.defaultSession.setPermissionCheckHandler(() => false);
  session.defaultSession.webRequest.onHeadersReceived((details, callback) => {
    // The desktop bundles its viewer code and reads simulator data through IPC.
    if (details.url.startsWith("wendy://app/")) {
      callback({
        responseHeaders: {
          ...details.responseHeaders,
          "Content-Security-Policy": [
            "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'",
          ],
        },
      });
    } else callback({});
  });

  window = new BrowserWindow({
    width: 1480,
    height: 960,
    minWidth: 1000,
    minHeight: 720,
    backgroundColor: "#111416",
    title: "Wendy Desktop",
    webPreferences: {
      preload: join(root, "electron/preload.cjs"),
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
      webviewTag: false,
    },
  });
  window.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  window.webContents.on("will-navigate", (event) => event.preventDefault());
  window.webContents.on("will-attach-webview", (event) =>
    event.preventDefault(),
  );

  // Finder-launched applications do not inherit a login shell's PATH. Preserve
  // the current Docker context/environment and add common CLI installation paths.
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([, value]) => typeof value === "string",
    ),
  );
  env.PATH = [
    ...new Set([
      join(root, "desktop-resources"),
      ...(env.PATH || "").split(delimiter),
      "/opt/homebrew/bin",
      "/usr/local/bin",
      join(homedir(), ".local/bin"),
      "/usr/bin",
      "/bin",
    ]),
  ].join(delimiter);
  env.TERM = "xterm-256color";
  delete env.ELECTRON_RUN_AS_NODE;
  delete env.WENDY_AGENT_SOCKET;
  const emit = (event) => {
    if (window && !window.isDestroyed())
      window.webContents.send("wendy:session-event", event);
  };
  sessions = new Sessions({ spawn: pty.spawn, emit, env, cwd: homedir() });
  service = new DesktopService({
    resources: join(root, "desktop-resources"),
    data: app.getPath("userData"),
    env,
    sessions,
    chooseDirectory: async () => {
      const result = await dialog.showOpenDialog(window, {
        title: "Open a Wendy project",
        properties: ["openDirectory", "createDirectory"],
      });
      return result.canceled ? null : result.filePaths[0];
    },
    confirmFlash: async (plan) => {
      const target = plan.target
        ? `${plan.target.id} · ${plan.target.name} · ${(plan.target.capacity_bytes / 1e9).toFixed(1)} GB`
        : plan.device_type;
      const answer = await dialog.showMessageBox(window, {
        type: "warning",
        title: "Flash device",
        message: `Erase and install WendyOS on ${target}?`,
        detail: `${plan.erase_scope}\n\nBoard: ${plan.device_type}\nVersion: ${plan.version}\n\nThe installer will ask for networking and any administrator access it needs.`,
        buttons: ["Cancel", "Open installer"],
        defaultId: 0,
        cancelId: 0,
        noLink: true,
      });
      return answer.response === 1;
    },
  });
  await service.initialize();
  window.webContents.on("render-process-gone", () => service.close());
  window.webContents.on("did-start-navigation", (_event, _url, _inPlace, isMainFrame) => {
    if (isMainFrame) service.close();
  });
  let markRendererReady;
  const rendererReady = new Promise((resolve) => {
    markRendererReady = resolve;
  });
  let viewerFrames = 0;
  const handlers = {
    status: () => {
      markRendererReady();
      return service.status();
    },
    containers: (runtime) => service.containers(runtime),
    "device:snapshot": (args) => service.deviceSnapshot(args),
    "device:logs:start": (args) => service.deviceLogsStart(args),
    "device:logs:snapshot": (id) => service.deviceLogsSnapshot(id),
    "device:logs:stop": (id) => service.deviceLogsStop(id),
    "vm:list": () => service.virtualMachines(),
    "project:open": () => service.pickProject(),
    "project:action": (args) => service.projectAction(args),
    "runtime:start-apple": () => service.startApple(),
    "sim:list": () =>
      Promise.all(service.simulators.map((sim) => service.simStatus(sim.id))),
    "sim:create": (args) => service.createSimulator(args),
    "sim:action": (args) => service.simulatorAction(args),
    "sim:request": async (args) => {
      const result = await service.simulatorRequest(args);
      if (args.path === "/api/scene/state") viewerFrames++;
      return result;
    },
    "install:drives": () => service.drives(),
    "install:plan": (args) => service.plan(args),
    "install:flash": (id) => service.flash(id),
    "install:verify": (args) => service.verify(args),
    discover: () => service.discover(),
    "session:list": () => sessions.list(),
    "session:snapshot": (id) => sessions.snapshot(id),
    "session:input": ({ id, data }) => sessions.write(id, data),
    "session:resize": ({ id, cols, rows }) => sessions.resize(id, cols, rows),
    "session:cancel": (id) => sessions.cancel(id),
  };
  for (const [method, handler] of Object.entries(handlers)) {
    ipcMain.handle(`wendy:${method}`, (event, args) => {
      if (!trustedSender(event, window.webContents, devURL))
        throw new Error("Untrusted desktop request.");
      return handler(args);
    });
  }
  window.on("close", (event) => {
    if (quitting || !sessions.list().some((s) => s.running)) return;
    event.preventDefault();
    void dialog
      .showMessageBox(window, {
        type: "warning",
        message: "Close active sessions?",
        detail:
          "Chat and build processes will stop. Interrupting an active flash can leave the drive incomplete. Simulator containers keep running.",
        buttons: ["Keep open", "Close sessions and quit"],
        defaultId: 0,
        cancelId: 0,
      })
      .then((answer) => {
        if (answer.response === 1) {
          quitting = true;
          app.quit();
        }
      });
  });
  // A locally repackaged app can keep the same URL across builds. Always load
  // its current entry point instead of an older Chromium cache entry.
  await window.loadURL(devURL || `${APP_URL}?launch=${Date.now()}`);
  if (process.env.WENDY_DESKTOP_SMOKE === "1") {
    // Integration smoke runs the real preload and native bridge without clicking
    // destructive controls. It exits so CI does not leave a desktop process open.
    // The React workspace calls status on mount. This detects a broken bundle
    // or CSP before the smoke script makes its own bridge request.
    await Promise.race([
      rendererReady,
      delay(10_000).then(() => {
        throw new Error("Desktop renderer did not mount.");
      }),
    ]);
    const result = await window.webContents.executeJavaScript(
      "window.wendyDesktop.status()",
    );
    console.log("DESKTOP_SMOKE " + JSON.stringify(result));
    const terminal = sessions.start("CLI terminal check", [
      service.cliSpec(["chat", "--list-profiles"]),
    ]);
    const deadline = Date.now() + 10_000;
    while (sessions.get(terminal.id).running && Date.now() < deadline)
      await delay(25);
    const output = sessions.snapshot(terminal.id);
    if (
      output.running ||
      output.exitCode !== 0 ||
      !output.output.includes("general")
    )
      throw new Error(`Desktop terminal failed: ${output.output}`);
    console.log("DESKTOP_TERMINAL_SMOKE passed");
    if (process.env.WENDY_DESKTOP_SMOKE_VIEWER === "1") {
      const deadline = Date.now() + 20_000;
      while (viewerFrames < 2 && Date.now() < deadline) await delay(100);
      if (viewerFrames < 2)
        throw new Error("Integrated viewer did not stream scene poses.");
      console.log("DESKTOP_VIEWER_SMOKE passed");
    }
    quitting = true;
    app.quit();
  }
}

app.on("window-all-closed", () => app.quit());
app.on("will-quit", () => {
  service?.close();
  sessions?.close();
});
