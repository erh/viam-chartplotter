import { execSync } from "node:child_process";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

// Build-time tile-cache-bust version, injected as __GIT_HASH__ and sent as
// `v=` on every tile URL. Note this only busts the *browser's* cache: the
// server keys its tile cache and ETag on ENCRenderRulesVersion (render/enc.go),
// so a renderer change needs that bump too or a new v= just refetches the
// same cached PNGs.
//
// Release builds: the nearest release tag from `git describe` — "0.4.9" on
// the tagged commit the Viam cloud build checks out, "0.4.9-3-g893001df4d62"
// past it, the bare sha if no tag is reachable.
//
// Dirty trees (uncommitted changes to the web or Go render sources, i.e.
// you're iterating) get "-dirty-{ts}" appended so every rebuild/reload and
// every `vite dev` restart yields a new value — otherwise OL keeps serving
// its in-memory tile cache from the previous run. The old check ran
// `git status --porcelain` over the whole tree, which the Viam cloud build
// always reported dirty (its npm install rewrites package-lock.json before
// vite runs), so every published build shipped "<sha>-dirty-<ts>" instead of
// its version. Hence the explicit pathspec: untracked files, mode-only
// changes and the lockfile are ignored.
//
// `vite dev` always appends the timestamp. "dev-{ts}" only if git isn't
// available at all.
function buildVersion(command: "build" | "serve"): string {
  const run = (cmd: string) =>
    execSync(cmd, { stdio: ["ignore", "pipe", "ignore"] })
      .toString()
      .trim();
  try {
    const described = run(
      "git describe --tags --match '[0-9]*.[0-9]*.[0-9]*' --always --abbrev=12"
    );
    const dirty = run(
      "git -c core.fileMode=false status --porcelain --untracked-files=no -- " +
        "src index.html vite.config.ts package.json tailwind.config.js svelte.config.js " +
        "render weather ':(glob)*.go' go.mod"
    );
    if (dirty) return `${described}-dirty-${Date.now()}`;
    return command === "serve" ? `${described}-${Date.now()}` : described;
  } catch {
    return `dev-${Date.now()}`;
  }
}

export default defineConfig(({ command }) => ({
  plugins: [svelte()],
  base: process.env.NODE_ENV === "production" ? "/viam-chartplotter" : "",
  css: {
    postcss: false,
  },
  define: {
    __GIT_HASH__: JSON.stringify(buildVersion(command)),
  },
  server: {
    proxy: {
      // Forward backend routes to the Go module (run via `make run`) so the
      // dev server on :5173 behaves like the bundled production server.
      "/noaa-wms": "http://localhost:8888",
      "/noaa-enc": "http://localhost:8888",
      "/noaa-weather": "http://localhost:8888",
      "/app-config": "http://localhost:8888",
      "/version": "http://localhost:8888",
      "/myboat-icon": "http://localhost:8888",
    },
  },
}));
