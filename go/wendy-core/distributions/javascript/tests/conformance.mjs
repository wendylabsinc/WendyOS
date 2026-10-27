// Executed verbatim in Node.js and an actual browser against the real engine.
export function assertCore(core) {
  const exports = Object.keys(core).sort();
  if (JSON.stringify(exports) !== '["WendyCoreVersion","WendyHelloWorld"]') {
    throw new Error(`Unexpected Core exports: ${exports}`);
  }
  const version = core.WendyCoreVersion();
  if (typeof version !== 'string' || !/^\d+\.\d+\.\d+(?:[-+].+)?$/.test(version)) {
    throw new Error(`Invalid WendyCoreVersion: ${version}`);
  }
  if (globalThis.process?.env?.WENDY_CORE_VERSION && version !== process.env.WENDY_CORE_VERSION) {
    throw new Error(`Wrong engine version: ${version}`);
  }
  for (let i = 0; i < 100; i++) {
    if (core.WendyCoreVersion() !== version) throw new Error("Unstable version");
    const value = core.WendyHelloWorld();
    if (value !== 42 || !Number.isInteger(value)) {
      throw new Error(`WendyHelloWorld returned ${value}`);
    }
  }
  return true;
}
