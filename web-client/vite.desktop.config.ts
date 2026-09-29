import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

export default defineConfig({
  root: fileURLToPath(new URL("./desktop", import.meta.url)),
  base: "./",
  plugins: [react()],
  resolve: { alias: { "@": fileURLToPath(new URL(".", import.meta.url)) } },
  build: { outDir: "../dist-desktop", emptyOutDir: true },
  server: { host: "127.0.0.1", port: 5174, strictPort: true },
});
