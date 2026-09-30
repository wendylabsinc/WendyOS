import {
  App,
  applyDocumentTheme,
  applyHostStyleVariables,
} from "@modelcontextprotocol/ext-apps";
import { OpenAIExtensions } from "@openai/mcp-extensions/app";
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
  options?: { timeout?: number },
) {
  let r;
  try {
    r = await app.callServerTool({ name, arguments: args }, options);
  } catch (e) {
    if (String(e).includes("trusted tool scope")) {
      window.dispatchEvent(new Event("wendy:refresh-connection"));
      throw new ToolScopeError(
        "Open ChatGPT Plugins, select Wendy, and click Refresh in its connection details. Then open Wendy in a new conversation.",
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
