export type SceneSession = {
  url: string;
  token: string;
  expires_at: string;
  resource_uri?: string;
};

export type SceneBridge = {
  read: (
    sessionId: string,
    part: "geometry" | "state",
    signal: AbortSignal,
    afterSequence?: number,
  ) => Promise<{ _meta?: Record<string, unknown> }>;
  close: (sessionId: string) => Promise<unknown>;
  pause?: (sessionId: string) => Promise<unknown>;
};

export function sceneSession(value: unknown): SceneSession | undefined {
  if (value === undefined) return undefined;
  if (!value || typeof value !== "object")
    throw Error("Invalid scene session.");
  const session = value as Record<string, unknown>;
  if (
    typeof session.url !== "string" ||
    typeof session.token !== "string" ||
    !/^[a-f0-9-]{36}$/.test(session.token) ||
    typeof session.expires_at !== "string" ||
    !Number.isFinite(Date.parse(session.expires_at))
  )
    throw Error("Invalid scene session.");
  const url = new URL(session.url);
  if (
    url.protocol !== "http:" ||
    url.hostname !== "127.0.0.1" ||
    !url.port ||
    url.username ||
    url.password ||
    url.pathname !== "/" ||
    url.search ||
    url.hash
  ) {
    throw Error("The scene session is not on this laptop.");
  }
  if (
    session.resource_uri !== undefined &&
    session.resource_uri !== `wendy://simulator-scenes/${session.token}`
  )
    throw Error("Invalid scene resource.");
  return {
    url: url.origin,
    token: session.token,
    expires_at: session.expires_at,
    ...(session.resource_uri !== undefined && {
      resource_uri: session.resource_uri as string,
    }),
  };
}

export function closeSceneSession(
  session?: SceneSession,
  bridge?: SceneBridge,
) {
  if (!session) return;
  if (session.resource_uri && bridge) {
    void bridge.close(session.token).catch(() => {});
    return;
  }
  void fetch(`${session.url}/session`, {
    method: "DELETE",
    headers: { Authorization: `Bearer ${session.token}` },
    credentials: "omit",
    signal: AbortSignal.timeout(3000),
  }).catch(() => {});
}

async function unpackScene(encoded: string, signal: AbortSignal) {
  const limit = 32 << 20;
  if (encoded.length > Math.ceil(limit / 3) * 4)
    throw Error("Compressed scene exceeds the response limit.");
  const bytes = Uint8Array.from(atob(encoded), (value) => value.charCodeAt(0));
  const reader = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip")).getReader();
  const abort = () => { void reader.cancel(signal.reason).catch(() => {}); };
  signal.addEventListener("abort", abort, { once: true });
  const chunks: Uint8Array<ArrayBuffer>[] = [];
  let size = 0;
  try {
    signal.throwIfAborted();
    for (;;) {
      const { done, value } = await reader.read();
      signal.throwIfAborted();
      if (done) break;
      size += value.byteLength;
      if (size > limit) throw Error("Scene exceeds the 32 MiB response limit.");
      chunks.push(value);
    }
    return JSON.parse(await new Blob(chunks).text());
  } finally {
    signal.removeEventListener("abort", abort);
    await reader.cancel().catch(() => {});
  }
}

export async function readScene(
  session: SceneSession,
  path: string,
  signal: AbortSignal,
  bridge?: SceneBridge,
  afterSequence?: number,
) {
  if (!["/api/scene", "/api/scene/state"].includes(path)) {
    throw Error("Unsupported scene resource.");
  }
  if (Date.now() >= Date.parse(session.expires_at)) {
    throw Error("The viewer session expired. Choose Reconnect view.");
  }
  if (session.resource_uri) {
    if (!bridge)
      throw Error(
        "This host cannot read the simulation scene. Open it in your browser.",
      );
    const boundedSignal = AbortSignal.any([signal, AbortSignal.timeout(path === "/api/scene" ? 45_000 : 15_000)]);
    const result = await bridge.read(
      session.token,
      path === "/api/scene" ? "geometry" : "state",
      boundedSignal,
      afterSequence,
    );
    if (signal.aborted) throw signal.reason;
    const data = typeof result._meta?.scene_data_gzip === "string"
      ? await unpackScene(result._meta.scene_data_gzip, boundedSignal)
      : result._meta?.scene_data;
    boundedSignal.throwIfAborted();
    if (!data || typeof data !== "object" || Array.isArray(data))
      throw Error("The simulator returned no scene data.");
    return data;
  }
  const response = await fetch(session.url + path, {
    headers: { Authorization: `Bearer ${session.token}` },
    cache: "no-store",
    credentials: "omit",
    redirect: "error",
    signal: AbortSignal.any([signal, AbortSignal.timeout(15_000)]),
  });
  if (response.status === 401 || response.status === 403) {
    throw Error("The viewer session ended. Choose Reconnect view.");
  }
  if (!response.ok)
    throw Error("The simulator is not responding. Reconnecting...");
  return response.json();
}
