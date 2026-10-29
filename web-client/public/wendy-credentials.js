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
  const get = (key) => transaction("readonly", (store) => store.get(key));
  // One sign-in per organization: records are keyed "<tenant>:<subject>", the
  // same pair the wrapped seed's aad carries. "current" names the slot the
  // last sign-in used, so a new worker restores it. "session" is the single
  // slot from before; the first load moves it to its organization's slot.
  const CURRENT = "current";
  const LEGACY = "session";
  let current;
  const slotOf = (aad) => {
    if (!aad.startsWith("operator-key:")) throw new Error("Invalid key label");
    return aad.slice("operator-key:".length);
  };
  const sessionOf = (record) =>
    typeof record === "string" ? record : (record?.session ?? "");
  async function migrate() {
    const legacy = await get(LEGACY);
    if (legacy === "") return;
    let profile;
    try {
      profile = JSON.parse(sessionOf(legacy)).profile;
    } catch {}
    const slot =
      profile?.tenant && profile?.subject
        ? profile.tenant + ":" + profile.subject
        : "";
    await transaction("readwrite", (store) => {
      if (slot) {
        store.put(legacy, slot);
        store.put(slot, CURRENT);
      }
      return store.delete(LEGACY);
    });
  }
  async function currentSlot() {
    if (current) return current;
    await migrate();
    current = (await get(CURRENT)) || undefined;
    return current;
  }
  async function slots(tenant) {
    const keys = await transaction("readonly", (store) => store.getAllKeys());
    return keys.filter(
      (key) =>
        key !== CURRENT &&
        key !== LEGACY &&
        (!tenant || key.startsWith(tenant + ":")),
    );
  }
  // WDY-3920, as the console does: the operator key's ML-DSA seed is
  // AES-GCM-encrypted under a NON-extractable WebCrypto key stored next to it
  // as a CryptoKey. Page script cannot export that key, but script running in
  // this origin can still call decrypt, so this protects the seed at rest
  // (readable records, disk), not against XSS. aad is the tenant and operator.
  // A record that is a string comes from before this and holds the key in
  // plaintext; the worker rewraps it on its first restore.
  return {
    // The slot this worker uses, or undefined before the first load.
    slot: () => current,
    // Selects this worker's organization: subject picks one operator, tenant
    // alone the most recent sign-in to it. It reports whether one is saved.
    select: async (tenant, subject) => {
      await currentSlot();
      let slot = subject ? tenant + ":" + subject : "";
      if (!slot) {
        const saved = await slots(tenant);
        const last = await get(CURRENT);
        slot = saved.includes(last) ? last : (saved[0] ?? tenant + ":");
      }
      current = slot;
      if ((await get(slot)) === "") return false;
      await transaction("readwrite", (store) => store.put(slot, CURRENT));
      return true;
    },
    load: async () => {
      const slot = await currentSlot();
      return slot ? sessionOf(await get(slot)) : "";
    },
    loadKey: async (aad) => {
      const record = await get(slotOf(aad));
      if (typeof record?.session !== "string")
        throw new Error("No wrapped operator key");
      return new Uint8Array(
        await crypto.subtle.decrypt(
          {
            name: "AES-GCM",
            iv: record.iv,
            additionalData: new TextEncoder().encode(aad),
          },
          record.wrappingKey,
          record.wrappedSeed,
        ),
      );
    },
    saveWrapped: async (session, seed, aad) => {
      const slot = slotOf(aad);
      const wrappingKey = await crypto.subtle.generateKey(
        { name: "AES-GCM", length: 256 },
        false,
        ["encrypt", "decrypt"],
      );
      const iv = crypto.getRandomValues(new Uint8Array(12));
      const wrappedSeed = await crypto.subtle.encrypt(
        { name: "AES-GCM", iv, additionalData: new TextEncoder().encode(aad) },
        wrappingKey,
        seed,
      );
      // A token refresh rewrites the record; this browser's registry key stays.
      const previous = await get(slot);
      const { registryWrappingKey, registryIV, wrappedRegistryKey } =
        typeof previous === "object" && previous ? previous : {};
      await transaction("readwrite", (store) => {
        store.put(
          {
            session,
            wrappingKey,
            iv,
            wrappedSeed,
            registryWrappingKey,
            registryIV,
            wrappedRegistryKey,
          },
          slot,
        );
        return store.put(slot, CURRENT);
      });
      current = slot;
    },
    // This browser's registry encryption key (one per operator device, WDY-3968),
    // wrapped like the seed under its own non-extractable key, in the sign-in's
    // record: it survives token refreshes and goes with sign-out.
    loadRegistryKey: async (aad) => {
      const record = await get(slotOf(aad));
      if (!record?.wrappedRegistryKey) return undefined;
      return new Uint8Array(
        await crypto.subtle.decrypt(
          {
            name: "AES-GCM",
            iv: record.registryIV,
            additionalData: new TextEncoder().encode("registry-" + aad),
          },
          record.registryWrappingKey,
          record.wrappedRegistryKey,
        ),
      );
    },
    saveRegistryKey: async (aad, key) => {
      const slot = slotOf(aad);
      const registryWrappingKey = await crypto.subtle.generateKey(
        { name: "AES-GCM", length: 256 },
        false,
        ["encrypt", "decrypt"],
      );
      const registryIV = crypto.getRandomValues(new Uint8Array(12));
      const wrappedRegistryKey = await crypto.subtle.encrypt(
        {
          name: "AES-GCM",
          iv: registryIV,
          additionalData: new TextEncoder().encode("registry-" + aad),
        },
        registryWrappingKey,
        key,
      );
      const record = await get(slot);
      if (typeof record?.session !== "string")
        throw new Error("No saved sign-in for the registry key");
      await transaction("readwrite", (store) =>
        store.put(
          { ...record, registryWrappingKey, registryIV, wrappedRegistryKey },
          slot,
        ),
      );
    },
    clear: async () => {
      const slot = await currentSlot();
      if (slot) await transaction("readwrite", (store) => store.delete(slot));
    },
    // Removes every saved sign-in to tenant, or every saved sign-in.
    forget: async (tenant) => {
      await currentSlot();
      const doomed = await slots(tenant);
      if (!tenant) doomed.push(CURRENT);
      if (doomed.length)
        await transaction("readwrite", (store) => {
          let request;
          for (const key of doomed) request = store.delete(key);
          return request;
        });
    },
  };
})();

/* Pending redirects use their own database: no credential schema upgrade,
   organization slot, token or signing key is involved. Only Go sees plaintext. */
self.wendyPending = (() => {
  const LIFETIME = 10 * 60 * 1000;
  const MAX_RECORDS = 32;
  const MAX_BYTES = 8192; // browserauth.MaxRedirectRecord
  const validHandle = (handle) => /^[A-Za-z0-9_-]{43}$/.test(handle);
  const live = (record, now) =>
    Number.isSafeInteger(record?.created) &&
    record.created <= now && now < record.created + LIFETIME;
  const aad = (handle, created) =>
    new TextEncoder().encode(`wendy-pending-v1:${handle}:${created}`);
  function open() {
    return new Promise((resolve, reject) => {
      let settled = false;
      const request = indexedDB.open("wendy-auth-pending", 1);
      const fail = (error) => {
        settled = true;
        clearTimeout(timer);
        reject(error);
      };
      const timer = setTimeout(() => fail(new Error("Pending sign-in storage timed out")), 5000);
      request.onupgradeneeded = () => {
        if (settled) { request.transaction.abort(); return; }
        request.result.createObjectStore("pending");
      };
      request.onsuccess = () => {
        clearTimeout(timer);
        const db = request.result;
        if (settled) { db.close(); return; }
        settled = true;
        db.onversionchange = () => db.close();
        resolve(db);
      };
      request.onerror = () => fail(request.error);
      request.onblocked = () => fail(new Error("Pending sign-in storage is blocked"));
    });
  }
  async function transact(operation) {
    const db = await open();
    try {
      return await new Promise((resolve, reject) => {
        const tx = db.transaction("pending", "readwrite");
        let result;
        let failure;
        tx.oncomplete = () => resolve(result);
        tx.onerror = tx.onabort = () => reject(failure || tx.error || new Error("Pending sign-in storage was interrupted"));
        const store = tx.objectStore("pending");
        const now = Date.now();
        const kept = [];
        const cursor = store.openCursor();
        cursor.onsuccess = () => {
          const item = cursor.result;
          if (item) {
            if (!live(item.value, now)) item.delete();
            else kept.push({ handle: item.primaryKey, created: item.value.created });
            item.continue();
          } else {
            try { operation(store, kept, (value) => { result = value; }); }
            catch (error) { failure = error; tx.abort(); }
          }
        };
      });
    } finally { db.close(); }
  }
  function checkHandle(handle) {
    if (typeof handle !== "string" || !validHandle(handle))
      throw new Error("Invalid pending sign-in handle");
  }
  return {
    put: async (handle, bytes) => {
      checkHandle(handle);
      if (!(bytes instanceof Uint8Array) || bytes.length === 0 || bytes.length > MAX_BYTES)
        throw new Error("Invalid pending sign-in size");
      const created = Date.now();
      const wrappingKey = await crypto.subtle.generateKey(
        { name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"],
      );
      const iv = crypto.getRandomValues(new Uint8Array(12));
      const wrapped = await crypto.subtle.encrypt(
        { name: "AES-GCM", iv, additionalData: aad(handle, created) }, wrappingKey, bytes,
      );
      await transact((store, kept) => {
        // The oldest attempts make room rather than locking out every tab: one
        // whose tab closed at the IdP has no handle left to cancel it with.
        kept.sort((a, b) => a.created - b.created);
        for (const { handle } of kept.slice(0, Math.max(0, kept.length - MAX_RECORDS + 1)))
          store.delete(handle);
        // add refuses accidental handle reuse instead of replacing an attempt.
        store.add({ created, wrappingKey, iv, wrapped }, handle);
      });
    },
    take: async (handle) => {
      checkHandle(handle);
      const record = await transact((store, _kept, done) => {
        const request = store.get(handle);
        request.onsuccess = () => {
          done(request.result);
          store.delete(handle);
        };
      });
      if (!record) return undefined;
      // Decrypt only after deletion commits; two workers cannot consume it.
      if (!live(record, Date.now()) || record.wrappingKey?.extractable !== false ||
          !(record.wrapped instanceof ArrayBuffer) || record.wrapped.byteLength > MAX_BYTES + 16)
        throw new Error("Invalid pending sign-in record");
      return new Uint8Array(await crypto.subtle.decrypt(
        { name: "AES-GCM", iv: record.iv, additionalData: aad(handle, record.created) },
        record.wrappingKey, record.wrapped,
      ));
    },
    delete: async (handle) => {
      checkHandle(handle);
      await transact((store) => store.delete(handle));
    },
  };
})();
