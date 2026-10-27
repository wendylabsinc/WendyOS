import '../assets/wasm/wasm_exec.mjs';
import { createCore } from './interface.mjs';

const go = new globalThis.Go();
const key = `__wendyCoreReady_${crypto.randomUUID().replaceAll('-', '')}`;
go.argv = ['wendy-core', key];
let ready;
let failed;
const initialized = new Promise((resolve, reject) => { ready = resolve; failed = reject; });
globalThis[key] = ready;
let backend;
try {
  const response = await fetch(new URL('../assets/wasm/wendy_core.wasm', import.meta.url));
  if (!response.ok) throw new Error(`Wendy Core WASM fetch failed: ${response.status}`);
  // Works without requiring the host to configure application/wasm MIME.
  const { instance } = await WebAssembly.instantiate(await response.arrayBuffer(), go.importObject);
  go.run(instance).then(() => failed(new Error('Wendy Core exited during initialization')), failed);
  backend = await initialized;
} finally {
  delete globalThis[key];
}
const core = createCore(backend);
export const WendyHelloWorld = core.WendyHelloWorld;
export const WendyCoreVersion = core.WendyCoreVersion;
