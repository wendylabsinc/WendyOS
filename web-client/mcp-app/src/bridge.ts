import {
  App,
  applyDocumentTheme,
  applyHostStyleVariables,
} from "@modelcontextprotocol/ext-apps";
import { OpenAIExtensions } from "@openai/mcp-extensions/app";
import type { RequestOptions } from "@modelcontextprotocol/sdk/shared/protocol.js";
import { RequestQueue, type Priority } from "./request-queue";
import type { SceneBridge } from "./simulator-session";
const requests = new RequestQueue();
export const APP_WEB_REQUEST_OPTIONS = {
  timeout: 50_000,
  maxTotalTimeout: 50_000,
  resetTimeoutOnProgress: false,
};
export const app = new App({ name: "Wendy devices", version: "0.2.0" });
export const extensions = new OpenAIExtensions(app);
export const sceneBridge: SceneBridge = {
  read: (sessionId, part, signal, afterSequence) =>
    call(
      "simulator_scene_read",
      { session_id: sessionId, part, ...(part === "geometry" && { encoding: "gzip" }), ...(part === "state" && afterSequence !== undefined && { history: true, after_sequence: afterSequence }) },
      {
        signal,
        timeout: part === "geometry" ? 45_000 : 15_000,
        maxTotalTimeout: part === "geometry" ? 45_000 : 15_000,
        priority: "background",
      },
    ),
  close: (sessionId) =>
    call(
      "simulator_scene_close",
      { session_id: sessionId },
      { timeout: 3000, maxTotalTimeout: 3000, priority: "background" },
    ),
  pause: (sessionId) => call("simulator_scene_pause", { session_id: sessionId }, { timeout: 3000, maxTotalTimeout: 3000, priority: "background" }),
};
class ToolScopeError extends Error {}
export function toolErrorMessage(error: unknown): string {
  // The workspace owns one actionable banner for a stale host tool catalog.
  if (error instanceof ToolScopeError) return "";
  return error instanceof Error ? error.message : String(error);
}
export async function call(
  name: string,
  args: Record<string, unknown> = {},
  options?: Pick<
    RequestOptions,
    "timeout" | "maxTotalTimeout" | "resetTimeoutOnProgress" | "signal"
  > & { priority?: Priority },
) {
  let r;
  try {
    const { priority, ...requestOptions } = options ?? {};
    r = await requests.enqueue(
      () => app.callServerTool({ name, arguments: args }, requestOptions),
      priority,
      requestOptions.signal,
    );
  } catch (e) {
    if (String(e).includes("trusted tool scope")) {
      window.dispatchEvent(new Event("wendy:refresh-connection"));
      throw new ToolScopeError(
        "Open ChatGPT Plugins → Wendy → Manage app → Refresh tools. Then open Wendy in a new conversation.",
      );
    }
    throw e;
  }
  if (r.isError)
    throw Error(
      r.content
        .filter((c) => c.type === "text")
        .map((c) => c.text)
        .join("\n") || "Operation failed",
    );
  return r;
}
export function theme() {
  const c = app.getHostContext();
  if (c?.theme) {
    applyDocumentTheme(c.theme);
    document.documentElement.dataset.theme = c.theme;
  }
  if (c?.styles?.variables) applyHostStyleVariables(c.styles.variables);
  for (const edge of ["top", "right", "bottom", "left"] as const) {
    document.documentElement.style.setProperty(
      `--wendy-safe-${edge}`,
      `${Math.max(0, c?.safeAreaInsets?.[edge] ?? 0)}px`,
    );
  }
}
// Call directly from a user action, before waiting for a server tool. Hosts may
// retain inline mode; the simulator still opens in its focused layout.
export async function requestFullscreen() {
  const context = app.getHostContext();
  if (context?.displayMode === "fullscreen") return;
  if (context?.availableDisplayModes && !context.availableDisplayModes.includes("fullscreen"))
    return;
  try {
    await app.requestDisplayMode({ mode: "fullscreen" }, { timeout: 3000 });
  } catch {
    // Display-mode support must not prevent loading the simulation.
  }
}
export async function share(text: string, newChat = false) {
  const params = {
    role: "user" as const,
    content: [{ type: "text" as const, text }],
  };
  if (extensions.message)
    return extensions.message.send({
      ...params,
      _meta: {
        "openai/message": { target: newChat ? "new" : "active", send: true },
      },
    });
  if (newChat)
    throw Error(
      "This host does not support creating a conversation from the app.",
    );
  return app.sendMessage(params);
}
