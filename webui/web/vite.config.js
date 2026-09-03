import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5274,
    proxy: {
      "/api": { target: "http://127.0.0.1:4200", changeOrigin: true },
      "/agui": { target: "http://127.0.0.1:4200", changeOrigin: true },
      "/audit/stream": { target: "http://127.0.0.1:4200", changeOrigin: true },
    },
  },
  build: { outDir: "dist", emptyOutDir: true },
  test: {
    environment: "jsdom",
    globals: true,
  },
});
