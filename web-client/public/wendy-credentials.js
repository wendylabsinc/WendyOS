/* Origin-scoped credentials. This script runs only in the WASM worker. */
self.wendyCredentials = (() => {
  let opening;
  function open() {
    if (!opening)
      opening = new Promise((resolve, reject) => {
        const request = indexedDB.open("wendy-client", 1);
        request.onupgradeneeded = () =>
          request.result.createObjectStore("credentials");
        request.onsuccess = () => resolve(request.result);
        request.onerror = () => {
          opening = undefined;
          reject(request.error);
        };
        request.onblocked = () => {
          opening = undefined;
          reject(new Error("Browser storage is blocked"));
        };
      });
    return opening;
  }
  async function transaction(mode, operation) {
    const db = await open();
    return new Promise((resolve, reject) => {
      const tx = db.transaction("credentials", mode);
      const request = operation(tx.objectStore("credentials"));
      tx.oncomplete = () => resolve(request.result ?? "");
      tx.onerror = () => reject(tx.error);
      tx.onabort = () =>
        reject(tx.error || new Error("Credential storage was interrupted"));
    });
  }
  return {
    load: () => transaction("readonly", (store) => store.get("session")),
    save: (value) =>
      transaction("readwrite", (store) => store.put(value, "session")),
    clear: () => transaction("readwrite", (store) => store.delete("session")),
  };
})();
