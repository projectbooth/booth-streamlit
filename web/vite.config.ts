/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // Relative asset URLs. Under iframe-proxy the browser loads this page at
  // /iframe/streamlit/..., and booth-core forwards only the remainder of the path to the backend
  // (ADR 0069), so absolute /assets/... URLs would escape the module's prefix.
  base: "./",
  server: {
    proxy: {
      // Local dev only: the Go backend on :8080 (`go run ./cmd/streamlit`).
      "/healthz": process.env.BOOTH_STREAMLIT_DEV_BACKEND ?? "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/setupTests.ts"],
  },
});
