import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In development the API runs on :8080 (`make run`); Vite proxies to it so
// the browser sees a single origin, exactly like production.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: { "/api": "http://127.0.0.1:8080", "/healthz": "http://127.0.0.1:8080" },
  },
  build: {
    outDir: "dist",
    emptyOutDir: false, // keep dist/.keep for go:embed
    sourcemap: false,
    target: "es2022",
  },
});
