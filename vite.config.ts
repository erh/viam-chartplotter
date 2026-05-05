import { execSync } from "node:child_process";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

// Build-time tile-cache-bust version. Default = git short hash so every new
// commit auto-busts OpenLayers / browser HTTP cache. When the working tree
// has uncommitted changes (i.e. you're iterating in dev), a startup
// timestamp is appended so each `npm run dev` restart also produces a new
// value — otherwise OL would keep serving its in-memory tile cache from the
// previous run even though the renderer code has changed. Falls back to
// "dev-{ts}" if git isn't available.
function gitHash(): string {
  const run = (cmd: string) =>
    execSync(cmd, { stdio: ["ignore", "pipe", "ignore"] })
      .toString()
      .trim();
  try {
    const sha = run("git rev-parse --short=12 HEAD");
    const dirty = run("git status --porcelain");
    return dirty ? `${sha}-dirty-${Date.now()}` : sha;
  } catch {
    return `dev-${Date.now()}`;
  }
}

export default defineConfig({
  plugins: [svelte()],
  base: process.env.NODE_ENV === "production" ? "/viam-chartplotter" : "",
  css: {
    postcss: false,
  },
  define: {
    __GIT_HASH__: JSON.stringify(gitHash()),
  },
  server: {
    proxy: {
      // Forward backend routes to the Go module (run via `make run`) so the
      // dev server on :5173 behaves like the bundled production server.
      "/noaa-wms": "http://localhost:8888",
      "/noaa-enc": "http://localhost:8888",
      "/version": "http://localhost:8888",
      "/myboat-icon": "http://localhost:8888",
    },
  },
});
