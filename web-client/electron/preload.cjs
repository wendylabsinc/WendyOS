// Sandboxed Electron preloads use CommonJS, not ES module imports.
// eslint-disable-next-line @typescript-eslint/no-require-imports
const { contextBridge, ipcRenderer } = require("electron");

// No raw IPC, filesystem, command line, environment, or Electron event objects
// cross this boundary. Main also validates every sender and request argument.
contextBridge.exposeInMainWorld("wendyDesktop", {
  status: () => ipcRenderer.invoke("wendy:status"),
  containers: (runtime) => ipcRenderer.invoke("wendy:containers", runtime),
  deviceSnapshot: (args) => ipcRenderer.invoke("wendy:device:snapshot", args),
  deviceLogsStart: (args) => ipcRenderer.invoke("wendy:device:logs:start", args),
  deviceLogsSnapshot: (id) => ipcRenderer.invoke("wendy:device:logs:snapshot", id),
  deviceLogsStop: (id) => ipcRenderer.invoke("wendy:device:logs:stop", id),
  virtualMachines: () => ipcRenderer.invoke("wendy:vm:list"),
  openProject: () => ipcRenderer.invoke("wendy:project:open"),
  projectAction: (args) => ipcRenderer.invoke("wendy:project:action", args),
  startApple: () => ipcRenderer.invoke("wendy:runtime:start-apple"),
  simulators: () => ipcRenderer.invoke("wendy:sim:list"),
  createSimulator: (args) => ipcRenderer.invoke("wendy:sim:create", args),
  simulatorAction: (args) => ipcRenderer.invoke("wendy:sim:action", args),
  simulatorRequest: (args) => ipcRenderer.invoke("wendy:sim:request", args),
  drives: () => ipcRenderer.invoke("wendy:install:drives"),
  plan: (args) => ipcRenderer.invoke("wendy:install:plan", args),
  flash: (id) => ipcRenderer.invoke("wendy:install:flash", id),
  verify: (args) => ipcRenderer.invoke("wendy:install:verify", args),
  discover: () => ipcRenderer.invoke("wendy:discover"),
  sessions: () => ipcRenderer.invoke("wendy:session:list"),
  snapshot: (id) => ipcRenderer.invoke("wendy:session:snapshot", id),
  input: (id, data) => ipcRenderer.invoke("wendy:session:input", { id, data }),
  resize: (id, cols, rows) =>
    ipcRenderer.invoke("wendy:session:resize", { id, cols, rows }),
  cancel: (id) => ipcRenderer.invoke("wendy:session:cancel", id),
  onSession: (callback) => {
    const listener = (_event, data) => callback(data);
    ipcRenderer.on("wendy:session-event", listener);
    return () => ipcRenderer.removeListener("wendy:session-event", listener);
  },
});
