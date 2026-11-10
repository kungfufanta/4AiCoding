import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `pnpm dev` proxies the API to a local sandboxd (compose publishes it on
// 127.0.0.1:9090). Override with SANDBOXD_API=http://host:port.
const api = process.env.SANDBOXD_API ?? "http://127.0.0.1:9090";

export default defineConfig({
  plugins: [react()],
  server: { proxy: { "/v1": { target: api, changeOrigin: true } } },
  build: { outDir: "dist", sourcemap: false },
});
