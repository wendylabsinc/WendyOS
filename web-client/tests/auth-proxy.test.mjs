import assert from "node:assert/strict";
import test from "node:test";
import { GET, POST } from "../app/api/wendy-auth/route.ts";

const auth = "https://auth.dev.wendy.sh";
function request(endpoint, method = "GET") {
  return new Request(`http://localhost:5173/api/wendy-auth?url=${encodeURIComponent(endpoint)}`, { method });
}

test("auth proxy rejects unsupported methods before forwarding", async (t) => {
  const upstream = t.mock.method(globalThis, "fetch", () => {
    throw new Error("Forbidden requests must not reach upstream");
  });
  for (const method of ["PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]) {
    for (const action of ["token", "revoke"]) {
      const response = await POST(request(`${auth}/realms/system/oauth2/${action}`, method));
      assert.equal(response.status, 403, `${method} ${action}`);
    }
  }
  assert.equal(upstream.mock.callCount(), 0);
});

test("auth proxy preserves exact endpoint and method permissions", async (t) => {
  const upstream = t.mock.method(globalThis, "fetch", async () => new Response("ok"));
  for (const [endpoint, method] of [
    [`${auth}/api/login/realm`, "POST"],
    [`${auth}/realms/system/oauth2/token`, "POST"],
    [`${auth}/realms/system/oauth2/revoke`, "POST"],
    [`${auth}/realms/system/userinfo`, "GET"],
    [`${auth}/realms/system/.well-known/openid-configuration`, "GET"],
    [`${auth}/realms/system/.well-known/jwks.json`, "GET"],
    ["https://identity.dev.pki.wendy.sh/v1/identity/certificate", "POST"],
    ["https://api.dev.wendy.sh/.well-known/wendy-cloud-grants/jwks.json", "GET"],
  ]) {
    const before = upstream.mock.callCount();
    const response = await (method === "GET" ? GET : POST)(request(endpoint, method));
    assert.equal(response.status, 200, endpoint);
    assert.equal(upstream.mock.callCount(), before + 1);
    const [url, options] = upstream.mock.calls.at(-1).arguments;
    assert.equal(url.href, endpoint);
    assert.equal(options.method, method);
    assert.equal(options.redirect, "manual");
    const wrongMethod = method === "GET" ? "POST" : "GET";
    assert.equal((await GET(request(endpoint, wrongMethod))).status, 403);
    assert.equal(upstream.mock.callCount(), before + 1);
  }
});

test("auth proxy rejects untrusted destinations", async (t) => {
  const upstream = t.mock.method(globalThis, "fetch", () => {
    throw new Error("Forbidden requests must not reach upstream");
  });
  for (const endpoint of [
    "https://attacker.test/realms/system/oauth2/token",
    `${auth}/realms/system/oauth2/token?redirect=evil`,
    `${auth}/realms/system/oauth2/token#fragment`,
    "https://user:password@auth.dev.wendy.sh/realms/system/oauth2/token",
  ]) {
    assert.equal((await POST(request(endpoint, "POST"))).status, 403);
  }
  assert.equal(upstream.mock.callCount(), 0);
});
