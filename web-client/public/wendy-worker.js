/* Credentials stay in this origin's IndexedDB and this worker. Its sibling
   files load relative to its own URL, so a page can serve the release from any
   directory. ?sha256=<hex> pins the decompressed WASM module. */
importScripts("wasm_exec.js", "wendy-credentials.js");
const reply = self.postMessage.bind(self);
const requests = new Map();
let internalID = 0;
// Keep private credential operations inside the worker. Only public profiles
// and RPC responses cross the postMessage boundary.
self.postMessage = (data) => {
  const pending = requests.get(data.id);
  if (pending) {
    requests.delete(data.id);
    pending(data);
  } else reply(data);
};
// Bytes to sign cross as a Uint8Array argument, never inside the JSON.
function invoke(request) {
  return new Promise((resolve, reject) => {
    requests.set(request.id, resolve);
    try {
      const { bytes, ...params } = request.params ?? {};
      if (bytes instanceof Uint8Array)
        self.wendyRequest(JSON.stringify({ ...request, params }), bytes);
      else self.wendyRequest(JSON.stringify(request));
    } catch (error) {
      requests.delete(request.id);
      reject(error);
    }
  });
}
const sessions =
  typeof BroadcastChannel === "function"
    ? new BroadcastChannel("wendy-session")
    : null;
if (sessions)
  sessions.onmessage = ({ data }) => {
    // Another tab signed out of one organization ("" = all of them).
    const tenant = self.wendyCredentials.slot()?.split(":")[0];
    if (data?.signedOut === "" || (tenant && data?.signedOut === tenant))
      reply({ event: "auth-signed-out" });
  };
let queue = Promise.resolve();
self.onmessage = ({ data }) => {
  queue = queue
    .catch(() => {})
    .then(async () => {
      const run = async () => {
        // Other tabs may have rotated the refresh token or signed out.
        if (["cloud-discover", "cloud-connect"].includes(data.method)) {
          const restored = await invoke({
            id: --internalID,
            method: "auth-restore",
            params: {},
          });
          if (restored.error) {
            reply({ id: data.id, error: restored.error });
            return;
          }
          if (!restored.result) {
            reply({ event: "auth-signed-out" });
            reply({ id: data.id, error: "Sign in with Wendy first" });
            return;
          }
        }
        const response = await invoke(data);
        if (data.method === "auth-signout" && !response.error)
          sessions?.postMessage({ signedOut: data.params?.tenant ?? "" });
        reply(response);
      };
      try {
        const shared = [
          "auth-complete",
          "auth-redirect-complete",
          "auth-restore",
          "auth-signout",
          "cloud-discover",
          "cloud-connect",
          "credential",
          "sign",
          "registry-key",
          "registry-unwrap",
        ].includes(data.method);
        if (shared && self.navigator?.locks)
          await self.navigator.locks.request("wendy-credentials", run);
        else await run();
      } catch (error) {
        reply({ id: data.id, error: String(error) });
      }
    });
};
(async () => {
  try {
    let response;
    try {
      response = await fetch(new URL("wendy.wasm.gz", self.location));
    } catch {
      throw new Error(
        "Could not download the Wendy client. Check your connection and try again.",
      );
    }
    if (!response.ok)
      throw new Error(
        "Could not download the Wendy client (HTTP " + response.status + ").",
      );
    let bytes = new Uint8Array(await response.arrayBuffer());
    // Fetch decodes Content-Encoding automatically. Some hosts serve the gzip
    // file as raw bytes, so inspect the payload rather than its HTTP headers.
    if (bytes[0] === 0x1f && bytes[1] === 0x8b) {
      if (!self.DecompressionStream)
        throw new Error(
          "This browser cannot decompress the Wendy client. Use a recent browser.",
        );
      try {
        bytes = new Uint8Array(
          await new Response(
            new Blob([bytes])
              .stream()
              .pipeThrough(new DecompressionStream("gzip")),
          ).arrayBuffer(),
        );
      } catch {
        throw new Error(
          "The Wendy client download could not be decompressed. Reload and try again.",
        );
      }
    }
    if (
      bytes[0] !== 0 ||
      bytes[1] !== 0x61 ||
      bytes[2] !== 0x73 ||
      bytes[3] !== 0x6d
    )
      throw new Error(
        "The server did not return a valid Wendy WASM client. Reload and try again.",
      );
    const pin = new URL(self.location).searchParams.get("sha256");
    if (pin) {
      const digest = new Uint8Array(
        await crypto.subtle.digest("SHA-256", bytes),
      );
      const hex = Array.from(digest, (b) =>
        b.toString(16).padStart(2, "0"),
      ).join("");
      if (hex !== pin.toLowerCase()) throw new Error("sha256 mismatch");
    }
    const go = new Go();
    const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
    await go.run(instance);
  } catch (error) {
    self.postMessage({ fatal: error?.message ?? String(error) });
  }
})();
