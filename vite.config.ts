import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [svelte()],
  base: process.env.NODE_ENV === "production" ? "/viam-chartplotter" : "",
  css: {
    postcss: false,
  },
  server: {
    proxy: {
      // Forward NOAA caching proxy requests to the Go module (run via `make run`).
      "/noaa-wms": "http://localhost:8888",
    },
  },
});
