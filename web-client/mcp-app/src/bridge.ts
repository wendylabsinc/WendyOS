import {
  App,
  applyDocumentTheme,
  applyHostStyleVariables,
} from "@modelcontextprotocol/ext-apps";
import { OpenAIExtensions } from "@openai/mcp-extensions/app";
export const app = new App({ name: "Wendy devices", version: "0.2.0" });
export const extensions = new OpenAIExtensions(app);
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
      throw Error(
        "Refresh the Wendy connection in ChatGPT Plugins, then reopen Wendy to load its updated tools.",
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
