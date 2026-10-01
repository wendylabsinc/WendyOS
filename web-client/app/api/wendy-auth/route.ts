// Fixed Wendy authentication endpoints only. The worker creates DPoP proofs
// against the upstream URI. This route forwards HTTP to avoid browser CORS.
const authOrigin = "https://auth.dev.wendy.sh";
const identity = "https://identity.dev.pki.wendy.sh/v1/identity/certificate";
function allowed(url: URL, method: string) {
  if (method !== "GET" && method !== "POST") return false;
  if (url.username || url.password || url.search || url.hash) return false;
  if (
    url.href ===
    "https://api.dev.wendy.sh/.well-known/wendy-cloud-grants/jwks.json"
  )
    return method === "GET";
  if (url.href === identity) return method === "POST";
  if (url.origin !== authOrigin) return false;
  if (url.pathname === "/api/login/realm") return method === "POST";
  return method === "GET"
    ? /^\/realms\/[a-zA-Z0-9_-]+\/(userinfo|\.well-known\/(openid-configuration|jwks\.json))$/.test(
        url.pathname,
      )
    : /^\/realms\/[a-zA-Z0-9_-]+\/oauth2\/(token|revoke)$/.test(url.pathname);
}
async function proxy(request: Request) {
  const origin = new URL(request.url).origin;
  if (request.headers.get("origin") && request.headers.get("origin") !== origin)
    return new Response("Origin not allowed", { status: 403 });
  if (request.headers.get("sec-fetch-site") === "cross-site")
    return new Response("Origin not allowed", { status: 403 });
  let upstream: URL;
  try {
    upstream = new URL(new URL(request.url).searchParams.get("url") || "");
  } catch {
    return new Response("Invalid endpoint", { status: 400 });
  }
  if (!allowed(upstream, request.method))
    return new Response("Endpoint not allowed", { status: 403 });
  const headers = new Headers();
  for (const name of ["content-type", "accept", "authorization", "dpop"]) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  const body =
    request.method === "POST" ? await request.arrayBuffer() : undefined;
  if (body && body.byteLength > 64 * 1024)
    return new Response("Request too large", { status: 413 });
  try {
    const response = await fetch(upstream, {
      method: request.method,
      headers,
      body,
      redirect: "manual",
      cache: "no-store",
      signal: AbortSignal.timeout(25000),
    });
    if (response.status >= 300 && response.status < 400)
      throw new Error("Unexpected upstream redirect");
    const out = new Headers({
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    });
    for (const name of ["content-type", "dpop-nonce"]) {
      const value = response.headers.get(name);
      if (value) out.set(name, value);
    }
    return new Response(response.body, {
      status: response.status,
      headers: out,
    });
  } catch (error) {
    console.error("Wendy authentication transport:", String(error));
    return new Response("Wendy authentication service is unavailable", {
      status: 502,
      headers: { "Cache-Control": "no-store" },
    });
  }
}
export const GET = proxy;
export const POST = proxy;
