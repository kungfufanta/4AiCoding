import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// `pnpm dev` proxies API calls to a local portal backend (S1 mounts
// the BFF there). Override with PORTAL_API=http://host:port.
const api = process.env.PORTAL_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      "/api": { target: api, changeOrigin: true },
      "/auth": { target: api, changeOrigin: true },
      "/healthz": { target: api, changeOrigin: true },
      "/readyz": { target: api, changeOrigin: true },
    },
  },
  build: { outDir: "dist", sourcemap: false },
});
