//go:build js && wasm

package main

import (
	"bytes"
	"os"
	"strings"
	"syscall/js"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
)

// The real worker script against Node's WebCrypto and a Map-backed IndexedDB
// that structured-clones what it stores, the way the browser does.
func loadCredentialScript(t *testing.T) js.Value {
	t.Helper()
	src, err := os.ReadFile("../../../web-client/public/wendy-credentials.js")
	if err != nil {
		t.Fatal(err)
	}
	js.Global().Call("eval", `
globalThis.self = globalThis;
globalThis.fakeIDB = new Map();
globalThis.indexedDB = { open() {
  const request = {};
  setTimeout(() => {
    const store = {
      get: (k) => ({ result: structuredClone(fakeIDB.get(k)) }),
      put: (v, k) => { fakeIDB.set(k, structuredClone(v)); return { result: k }; },
      delete: (k) => { fakeIDB.delete(k); return { result: undefined }; },
      getAllKeys: () => ({ result: [...fakeIDB.keys()] }),
    };
    request.result = { transaction() { const tx = { objectStore: () => store }; setTimeout(() => tx.oncomplete()); return tx; } };
    request.onsuccess();
  });
  return request;
} };`)
	js.Global().Call("eval", string(src))
	return js.Global().Get("fakeIDB")
}

func TestCredentialStoreWrapsTheSeed(t *testing.T) {
	idb := loadCredentialScript(t)
	st := browserCredentialStore{}
	seed := bytes.Repeat([]byte{0x5a}, 32)
	const aad = "operator-key:tenant:operator"
	if err := st.SaveWrapped([]byte(`{"version":2}`), seed, aad); err != nil {
		t.Fatal(err)
	}
	record := idb.Call("get", "tenant:operator")
	key := record.Get("wrappingKey")
	if record.Get("session").String() != `{"version":2}` || key.Get("extractable").Bool() || key.Get("algorithm").Get("name").String() != "AES-GCM" || record.Get("iv").Length() != 12 {
		t.Fatal("record is not a session plus a non-extractable AES-GCM wrapped seed")
	}
	wrapped := make([]byte, record.Get("wrappedSeed").Get("byteLength").Int())
	js.CopyBytesToGo(wrapped, js.Global().Get("Uint8Array").New(record.Get("wrappedSeed")))
	if bytes.Contains(wrapped, seed[:16]) {
		t.Fatal("seed stored in plaintext")
	}
	if got, err := st.LoadKey(aad); err != nil || !bytes.Equal(got, seed) {
		t.Fatalf("unwrap: %x, %v", got, err)
	}
	if _, err := st.LoadKey("operator-key:tenant:another"); err == nil {
		t.Fatal("seed unwrapped for another operator")
	}
	if raw, err := st.Load(); err != nil || string(raw) != `{"version":2}` {
		t.Fatalf("load: %q, %v", raw, err)
	}
	if err := st.Save([]byte(`{"privateKey":"x"}`)); err == nil {
		t.Fatal("saved a session that carries its key")
	}
	if err := st.Clear(); err != nil || idb.Call("has", "tenant:operator").Bool() {
		t.Fatal("sign-out kept the record")
	}
}

func TestCredentialStoreReplacesAPlaintextRecord(t *testing.T) {
	idb := loadCredentialScript(t)
	st := browserCredentialStore{}
	legacy := `{"version":1,"privateKey":"-----BEGIN PRIVATE KEY-----\ntest-only\n-----END PRIVATE KEY-----\n","profile":{"tenant":"tenant","subject":"operator"}}`
	idb.Call("set", "session", legacy)
	if raw, err := st.Load(); err != nil || string(raw) != legacy {
		t.Fatalf("legacy record not readable for migration: %q, %v", raw, err)
	}
	if _, err := st.LoadKey("operator-key:tenant:operator"); err == nil {
		t.Fatal("a plaintext record has no wrapped key")
	}
	if err := st.SaveWrapped([]byte(`{"version":2}`), bytes.Repeat([]byte{1}, 32), "operator-key:tenant:operator"); err != nil {
		t.Fatal(err)
	}
	if record := idb.Call("get", "tenant:operator"); record.Type() == js.TypeString || strings.Contains(record.Get("session").String(), "PRIVATE KEY") {
		t.Fatal("plaintext record survived the rewrap")
	}
	if idb.Call("has", "session").Bool() {
		t.Fatal("the single-slot record was not moved to its organization")
	}
}

// This browser's registry key is wrapped in the sign-in's record: it survives
// the record rewrite of a token refresh, rotates on request and goes with
// sign-out.
func TestRegistryKeyLivesWithTheSignIn(t *testing.T) {
	idb := loadCredentialScript(t)
	st := browserCredentialStore{}
	const aad = "operator-key:tenant:operator"
	profile := browserauth.Profile{Tenant: "tenant", Subject: "operator"}
	if err := st.SaveWrapped([]byte(`{"version":2}`), bytes.Repeat([]byte{0x5a}, 32), aad); err != nil {
		t.Fatal(err)
	}
	if _, err := browserRegistryKey(profile, false, false); err == nil {
		t.Fatal("unwrap found a registry key before one was made")
	}
	made, err := browserRegistryKey(profile, true, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := made.Bytes()
	if record := idb.Call("get", "tenant:operator"); record.Get("registryWrappingKey").Get("extractable").Bool() {
		t.Fatal("registry key wrapped under an extractable key")
	} else if wrapped := js.Global().Get("Uint8Array").New(record.Get("wrappedRegistryKey")); bytes.Contains(goBytes(wrapped), raw[:16]) {
		t.Fatal("registry key stored in plaintext")
	}
	if err := st.SaveWrapped([]byte(`{"version":2,"refreshed":true}`), bytes.Repeat([]byte{0x5a}, 32), aad); err != nil {
		t.Fatal(err)
	}
	again, err := browserRegistryKey(profile, false, false)
	if err != nil || !bytes.Equal(again.PublicKey().Bytes(), made.PublicKey().Bytes()) {
		t.Fatalf("registry key lost by a session save: %v", err)
	}
	if seed, err := st.LoadKey(aad); err != nil || !bytes.Equal(seed, bytes.Repeat([]byte{0x5a}, 32)) {
		t.Fatalf("operator seed after the registry key: %v", err)
	}
	rotated, err := browserRegistryKey(profile, true, true)
	if err != nil || bytes.Equal(rotated.PublicKey().Bytes(), made.PublicKey().Bytes()) {
		t.Fatalf("rotate kept the key: %v", err)
	}
	if err := st.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := browserRegistryKey(profile, false, false); err == nil {
		t.Fatal("registry key survived sign-out")
	}
}

func goBytes(v js.Value) []byte {
	b := make([]byte, v.Length())
	js.CopyBytesToGo(b, v)
	return b
}
