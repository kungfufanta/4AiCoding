import { fileURLToPath } from "node:url";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// `pnpm dev` proxies the API to a local sandboxd (compose publishes it on
// 127.0.0.1:9090). Override with SANDBOXD_API=http://host:port.
const api = process.env.SANDBOXD_API ?? "http://127.0.0.1:9090";

// sonner injects its stylesheet through a runtime <style> element, which the
// CSP (style-src 'self') blocks. Drop that call; main.tsx imports
// sonner/dist/styles.css so the same rules ship in the bundled CSS file.
const sonnerNoInlineCss: Plugin = {
  name: "sonner-no-inline-css",
  enforce: "pre",
  transform(code, id) {
    if (!/[\\/]sonner[\\/]dist[\\/]index\.m?js$/.test(id)) return;
    return { code: code.replace(/^__insertCSS\(/m, "void ("), map: null };
  },
};

export default defineConfig({
  plugins: [sonnerNoInlineCss, react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  server: { proxy: { "/v1": { target: api, changeOrigin: true }, "/version": { target: api, changeOrigin: true } } },
  // never inline assets as data: URIs — the CSP (font-src defaults to 'self') blocks them
  build: { outDir: "dist", sourcemap: false, assetsInlineLimit: 0 },
});
