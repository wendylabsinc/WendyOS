import {
  App,
  applyDocumentTheme,
  applyHostStyleVariables,
} from "@modelcontextprotocol/ext-apps";
import { OpenAIExtensions } from "@openai/mcp-extensions/app";
import type { RequestOptions } from "@modelcontextprotocol/sdk/shared/protocol.js";
import { RequestQueue, type Priority } from "./request-queue";
const requests = new RequestQueue();
export const APP_WEB_REQUEST_OPTIONS = {
  timeout: 50_000,
  maxTotalTimeout: 50_000,
  resetTimeoutOnProgress: false,
};
export const app = new App({ name: "Wendy devices", version: "0.2.0" });
export const extensions = new OpenAIExtensions(app);
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
