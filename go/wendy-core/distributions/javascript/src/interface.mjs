// Generated from the C contract. Run make generate; do not edit.
export function createCore(backend) {
  return Object.freeze({
    WendyCoreVersion() { return backend.WendyCoreVersion(); },
    WendyHelloWorld() { return backend.WendyHelloWorld(); },
  });
}
