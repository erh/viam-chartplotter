<script lang="ts">
  // Routes panel: list/load/save/delete routes stored in the Viam location
  // metadata via the nav service (routes_* DoCommand, see nav_routes.go), plus
  // capture the recorded track between two times as a new route. The nav API is
  // read lazily through a getter (the robot client is assigned asynchronously
  // and isn't a reactive prop), so the panel always sees the current value.
  import type { PositionPoint } from "./BoatInfo";
  import { downloadGpx } from "./gpx";
  import { simplifyTrack, pathLengthMeters, type LatLng } from "./simplify";
  import {
    listRoutes,
    saveRoute,
    deleteRoute,
    renameRoute,
    sizeWarning,
    newRouteId,
    nextColor,
    type Route,
    type RoutesApi,
  } from "./routeStore";

  interface Props {
    getRoutesApi: () => RoutesApi | null;
    currentWaypoints: { id?: string; lat: number; lng: number }[];
    positionHistory: PositionPoint[];
    onLoadRoute: (waypoints: LatLng[]) => Promise<void>;
    // Optional: pull the recorded track for an explicit [t0,t1] window straight
    // from tabular data (beyond the in-memory positionHistory). When absent the
    // track form falls back to filtering positionHistory.
    fetchTrackWindow?: (t0: Date, t1: Date) => Promise<PositionPoint[]>;
    // Optional: preview route geometry on the map. Pass the set of routes to
    // draw; an empty array clears the overlay.
    onPreviewRoutes?: (routes: { waypoints: LatLng[]; color?: string }[]) => void;
  }

  let {
    getRoutesApi,
    currentWaypoints,
    positionHistory,
    fetchTrackWindow,
    onLoadRoute,
    onPreviewRoutes,
  }: Props = $props();

  const NM = 1852;

  let routes = $state<Route[]>([]);
  // Display order: alphabetical by name (case-insensitive), stable across loads.
  const sortedRoutes = $derived(
    [...routes].sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" }))
  );
  let loading = $state(false);
  let busy = $state(false);
  let error = $state<string | null>(null);
  let loaded = $state(false);

  // Inline forms.
  let saveCurrentOpen = $state(false);
  let saveCurrentName = $state("");
  let saveCurrentColor = $state("#ff8800");
  let renamingId = $state<string | null>(null);
  let renameValue = $state("");
  // The route whose options menu is expanded (also previewed on the map). Only
  // one is open at a time.
  let expandedId = $state<string | null>(null);
  // When on, every route is drawn on the map at once (so you see the ones in
  // the region you're looking at), instead of just the expanded one.
  let showAll = $state(false);

  // Track-capture form.
  let trackFormOpen = $state(false);
  let t0Str = $state("");
  let t1Str = $state("");
  let granularity = $state(200);
  let trackName = $state("");
  let trackColor = $state("#ff8800");
  // Track fetched on demand for the chosen window (null = not fetched yet, so
  // the preview falls back to the in-memory positionHistory).
  let windowPoints = $state<PositionPoint[] | null>(null);
  let fetching = $state(false);
  let fetchError = $state<string | null>(null);

  function routesAvailable(): boolean {
    return !!getRoutesApi();
  }

  // Push the right set of routes to the map: all of them when "show all" is on,
  // otherwise just the expanded one (or nothing). Re-run whenever the routes or
  // selection change so the overlay stays in sync.
  function applyMapPreview() {
    if (!onPreviewRoutes) return;
    if (showAll) {
      onPreviewRoutes(routes.map((r) => ({ waypoints: r.waypoints, color: r.color })));
    } else if (expandedId) {
      const r = routes.find((x) => x.id === expandedId);
      onPreviewRoutes(r ? [{ waypoints: r.waypoints, color: r.color }] : []);
    } else {
      onPreviewRoutes([]);
    }
  }

  // datetime-local <-> Date helpers (local time, no timezone suffix).
  function toLocalInput(d: Date): string {
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }
  function fromLocalInput(s: string): Date | null {
    if (!s) return null;
    const d = new Date(s);
    return isNaN(d.getTime()) ? null : d;
  }

  async function refresh() {
    const api = getRoutesApi();
    if (!api) {
      error = "Routes need a navigation-service connection.";
      return;
    }
    loading = true;
    error = null;
    try {
      routes = await listRoutes(api);
      loaded = true;
      applyMapPreview();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  async function withStore<T>(fn: (api: RoutesApi) => Promise<T>): Promise<T | undefined> {
    const api = getRoutesApi();
    if (!api) {
      error = "Routes need a navigation-service connection.";
      return undefined;
    }
    busy = true;
    error = null;
    try {
      const result = await fn(api);
      routes = await listRoutes(api);
      applyMapPreview();
      return result;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
      return undefined;
    } finally {
      busy = false;
    }
  }

  async function doLoad(route: Route, reverse = false) {
    // reverse = navigate the route the other way (last waypoint first).
    const waypoints = reverse ? [...route.waypoints].reverse() : route.waypoints;
    if (
      currentWaypoints.length > 0 &&
      !confirm(
        `Replace the current ${currentWaypoints.length} waypoint(s) with "${route.name}"${
          reverse ? " (reversed)" : ""
        }?`
      )
    ) {
      return;
    }
    busy = true;
    error = null;
    try {
      await onLoadRoute(waypoints);
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      busy = false;
    }
  }

  function openSaveCurrent() {
    saveCurrentName = "";
    saveCurrentColor = nextColor(routes);
    saveCurrentOpen = true;
  }

  async function commitSaveCurrent() {
    const name = saveCurrentName.trim();
    if (!name) return;
    const waypoints = currentWaypoints.map((w) => ({ lat: w.lat, lng: w.lng }));
    const now = new Date().toISOString();
    const route: Route = {
      id: newRouteId(),
      name,
      source: "manual",
      color: saveCurrentColor,
      createdAt: now,
      updatedAt: now,
      waypoints,
    };
    await withStore((api) => saveRoute(api, route));
    saveCurrentOpen = false;
  }

  // Change an existing route's color (persisted via routes_rename, which carries
  // the color field). The expanded preview re-renders in the new color.
  async function setColor(r: Route, color: string) {
    if (color === r.color) return;
    await withStore((api) => renameRoute(api, r.id, { color }, new Date().toISOString()));
  }

  function startRename(r: Route) {
    renamingId = r.id;
    renameValue = r.name;
  }
  async function commitRename(r: Route) {
    const name = renameValue.trim();
    if (name && name !== r.name) {
      await withStore((api) => renameRoute(api, r.id, { name }, new Date().toISOString()));
    }
    renamingId = null;
  }

  async function doDelete(r: Route) {
    if (!confirm(`Delete route "${r.name}"? This can't be undone.`)) return;
    if (expandedId === r.id) expandedId = null; // collapse the menu we're deleting
    await withStore((api) => deleteRoute(api, r.id)); // refresh re-applies the preview
  }

  // Overwrite a route's geometry with the current active waypoints, keeping its
  // id/name/etc. Updates the route where it already lives (see doRoutesSave).
  async function doReplaceWithCurrent(r: Route) {
    if (currentWaypoints.length < 2) {
      error = "Need at least 2 active waypoints to replace a route.";
      return;
    }
    if (
      !confirm(
        `Replace "${r.name}" (${r.stats?.count ?? r.waypoints.length} pts) with the current ${currentWaypoints.length} waypoint(s)?`
      )
    ) {
      return;
    }
    const updated: Route = {
      id: r.id,
      name: r.name,
      notes: r.notes,
      color: r.color,
      source: r.source,
      createdAt: r.createdAt,
      updatedAt: new Date().toISOString(),
      waypoints: currentWaypoints.map((w) => ({ lat: w.lat, lng: w.lng })),
    };
    await withStore((api) => saveRoute(api, updated));
  }

  // Clicking a route expands its options menu and previews it on the map;
  // clicking it again collapses. Only one route is open at a time.
  function toggleExpand(r: Route) {
    expandedId = expandedId === r.id ? null : r.id;
    applyMapPreview();
  }

  function toggleShowAll() {
    showAll = !showAll;
    applyMapPreview();
  }

  function openTrackForm() {
    const now = new Date();
    t1Str = toLocalInput(now);
    t0Str = toLocalInput(new Date(now.getTime() - 24 * 3600 * 1000));
    trackName = "";
    trackColor = nextColor(routes);
    windowPoints = null;
    fetchError = null;
    trackFormOpen = true;
  }

  // Changing the window invalidates any previously fetched track — the user
  // must re-fetch (or fall back to the in-memory filter) for the new span.
  $effect(() => {
    const _win = t0Str + "|" + t1Str;
    void _win;
    windowPoints = null;
    fetchError = null;
  });

  async function loadWindow() {
    if (!fetchTrackWindow) return;
    const t0 = fromLocalInput(t0Str);
    const t1 = fromLocalInput(t1Str);
    if (!t0 || !t1 || t1 <= t0) return;
    fetching = true;
    fetchError = null;
    try {
      windowPoints = await fetchTrackWindow(t0, t1);
    } catch (e) {
      fetchError = e instanceof Error ? e.message : String(e);
      windowPoints = null;
    } finally {
      fetching = false;
    }
  }

  // Points feeding the preview: the fetched window if loaded (authoritative for
  // the chosen span, including history older than positionHistory), else the
  // in-memory history filtered to the window.
  const effectivePoints = $derived.by(() => {
    const t0 = fromLocalInput(t0Str);
    const t1 = fromLocalInput(t1Str);
    if (!t0 || !t1 || t1 <= t0) return [] as PositionPoint[];
    if (windowPoints) return windowPoints;
    const lo = t0.getTime();
    const hi = t1.getTime();
    return positionHistory.filter((p) => {
      const t = p.ts instanceof Date ? p.ts.getTime() : new Date(p.ts).getTime();
      return t >= lo && t <= hi;
    });
  });

  // Live preview of the simplified track for the chosen window + granularity.
  const trackPreview = $derived.by(() => {
    if (!trackFormOpen) {
      return { waypoints: [] as LatLng[], capped: false, inWindow: 0, distanceNm: 0 };
    }
    const pts = effectivePoints.map((p) => ({ lat: p.lat, lng: p.lng }));
    const simp = simplifyTrack(pts, {
      granularityMeters: granularity,
      maxPoints: 500,
    });
    return {
      waypoints: simp.waypoints,
      capped: simp.capped,
      inWindow: pts.length,
      distanceNm: pathLengthMeters(simp.waypoints) / NM,
    };
  });

  // Push the preview polyline to the map while the form is open. On close,
  // restore whatever the route list wants shown (expanded route / show-all).
  $effect(() => {
    if (trackFormOpen && onPreviewRoutes) {
      onPreviewRoutes(
        trackPreview.waypoints.length ? [{ waypoints: trackPreview.waypoints, color: trackColor }] : []
      );
      return () => applyMapPreview();
    }
  });

  async function commitSaveTrack() {
    const name = trackName.trim();
    if (!name || trackPreview.waypoints.length < 2) return;
    const now = new Date().toISOString();
    const route: Route = {
      id: newRouteId(),
      name,
      source: "track",
      color: trackColor,
      createdAt: now,
      updatedAt: now,
      waypoints: trackPreview.waypoints,
    };
    await withStore((api) => saveRoute(api, route));
    trackFormOpen = false;
  }

  function fmtDate(iso: string): string {
    const d = new Date(iso);
    return isNaN(d.getTime()) ? "" : d.toLocaleDateString();
  }

  // GPX export is client-side only (build the file, trigger a download) — for
  // carrying a route to another plotter, e.g. Garmin/GPX/*.gpx on an SD card.
  function exportCurrentGpx() {
    const today = new Date().toISOString().slice(0, 10);
    downloadGpx(
      `Waypoints ${today}`,
      currentWaypoints.map((w) => ({ lat: w.lat, lng: w.lng }))
    );
  }
</script>

<div class="flex flex-col gap-2 p-2 text-sm text-white">
  <div class="flex items-center justify-between">
    <div class="font-bold">Routes</div>
    <button
      class="px-2 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40"
      onclick={refresh}
      disabled={loading || busy}
      title="Reload routes from the cloud"
    >
      {loading ? "…" : loaded ? "Refresh" : "Load"}
    </button>
  </div>

  {#if error}
    <div class="text-warning-dark text-xs border border-warning-medium rounded px-2 py-1">
      {error}
    </div>
  {/if}

  {#if !routesAvailable()}
    <div class="text-xs opacity-70">Connecting to the navigation service…</div>
  {:else}
    {#if loaded && routes.length === 0}
      <div class="text-xs opacity-60">No saved routes yet.</div>
    {/if}

    {#if routes.length > 0}
      <label class="flex items-center gap-1.5 text-xs opacity-80 cursor-pointer select-none">
        <input type="checkbox" checked={showAll} onchange={toggleShowAll} />
        Show all on map
      </label>
    {/if}

    <div class="flex flex-col">
      {#each sortedRoutes as r (r.id)}
        <div class="border-b border-dark/60 last:border-b-0">
          <!-- Dense row: swatch + name + distance. Click toggles the menu below. -->
          <div class="flex items-center gap-2 py-0.5">
            <span
              class="inline-block w-3 h-3 rounded-sm shrink-0"
              style="background:{r.color ?? '#888'}"
            ></span>
            {#if renamingId === r.id}
              <!-- svelte-ignore a11y_autofocus -->
              <input
                class="flex-1 bg-dark px-1 rounded text-white"
                bind:value={renameValue}
                autofocus
                onkeydown={(e) => e.key === "Enter" && commitRename(r)}
                onblur={() => commitRename(r)}
              />
            {:else}
              <button
                class="flex-1 min-w-0 text-left truncate hover:text-accent"
                title="Show on map / options"
                onclick={() => toggleExpand(r)}
              >
                <span class:font-bold={expandedId === r.id}>{r.name}</span>
                {#if r.scope === "parent"}
                  <span class="text-[10px] uppercase opacity-40" title="Inherited from a parent location — read-only here">
                    · inherited</span
                  >
                {/if}
              </button>
            {/if}
            <span class="text-xs opacity-50 shrink-0 tabular-nums">{r.stats?.distanceNm.toFixed(1)} nm</span>
          </div>

          {#if expandedId === r.id}
            <div class="flex flex-col gap-1 pb-1.5 pl-5">
              <div class="flex items-center gap-2 text-xs opacity-60">
                <span>{r.stats?.count} pts</span>
                <span>·</span>
                <span class="uppercase">{r.source}</span>
                {#if r.createdAt}<span>·</span><span>{fmtDate(r.createdAt)}</span>{/if}
                {#if r.scope !== "parent"}
                  <span>·</span>
                  <label class="flex items-center gap-1 cursor-pointer" title="Route color">
                    Color
                    <input
                      type="color"
                      class="w-6 h-5 bg-transparent border border-dark rounded cursor-pointer p-0 align-middle"
                      value={r.color ?? "#ff8800"}
                      disabled={busy}
                      onchange={(e) => setColor(r, (e.currentTarget as HTMLInputElement).value)}
                    />
                  </label>
                {/if}
              </div>
              <div class="flex flex-wrap gap-1 text-xs">
                <button
                  class="px-1.5 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40"
                  onclick={() => doLoad(r)}
                  disabled={busy}>Load</button
                >
                <button
                  class="px-1.5 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40"
                  onclick={() => doLoad(r, true)}
                  disabled={busy}
                  title="Load this route in reverse (navigate the other way)">Reverse</button
                >
                <button
                  class="px-1.5 py-0.5 border border-dark rounded hover:bg-dark"
                  onclick={() => downloadGpx(r.name, r.waypoints)}
                  title="Download this route as a GPX file (e.g. for a Garmin memory card)"
                  >GPX</button
                >
                <!-- Inherited (parent-location) routes are read-only here. -->
                {#if r.scope !== "parent"}
                  <button
                    class="px-1.5 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40"
                    onclick={() => doReplaceWithCurrent(r)}
                    disabled={busy || currentWaypoints.length < 2}
                    title="Overwrite this route with the current active waypoints"
                    >Replace w/ current</button
                  >
                  <button
                    class="px-1.5 py-0.5 border border-dark rounded hover:bg-dark"
                    onclick={() => startRename(r)}>Rename</button
                  >
                  <button
                    class="px-1.5 py-0.5 border border-dark rounded hover:bg-dark text-warning-dark"
                    onclick={() => doDelete(r)}
                    disabled={busy}>Delete</button
                  >
                {/if}
              </div>
            </div>
          {/if}
        </div>
      {/each}
    </div>

    {#if sizeWarning(routes)}
      <div class="text-warning-dark text-[11px]">
        Saved routes are getting large — consider deleting unused ones.
      </div>
    {/if}

    <!-- Save current active waypoints as a route. -->
    {#if saveCurrentOpen}
      <div class="flex flex-col gap-1 border border-dark rounded px-2 py-1">
        <div class="text-xs opacity-70">Save {currentWaypoints.length} active waypoint(s)</div>
        <input
          class="bg-dark px-1 rounded text-white"
          placeholder="Route name"
          bind:value={saveCurrentName}
        />
        <label class="text-xs flex items-center gap-1.5">
          Color
          <input
            type="color"
            class="w-6 h-5 bg-transparent border border-dark rounded cursor-pointer p-0"
            bind:value={saveCurrentColor}
          />
        </label>
        <div class="flex gap-2 text-xs">
          <button
            class="px-2 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40"
            onclick={commitSaveCurrent}
            disabled={busy || !saveCurrentName.trim()}>Save</button
          >
          <button
            class="px-2 py-0.5 border border-dark rounded hover:bg-dark"
            onclick={() => (saveCurrentOpen = false)}>Cancel</button
          >
        </div>
      </div>
    {:else}
      <button
        class="px-2 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40 text-xs"
        onclick={openSaveCurrent}
        disabled={busy || currentWaypoints.length === 0}
        title={currentWaypoints.length === 0 ? "No active waypoints" : ""}
      >
        Save current waypoints…
      </button>
    {/if}

    <button
      class="px-2 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40 text-xs"
      onclick={exportCurrentGpx}
      disabled={currentWaypoints.length === 0}
      title={currentWaypoints.length === 0
        ? "No active waypoints"
        : "Download the current waypoints as a GPX route file (e.g. for a Garmin memory card)"}
    >
      Download current as GPX
    </button>

    <!-- Capture recorded track as a route. -->
    {#if trackFormOpen}
      <div class="flex flex-col gap-1 border border-dark rounded px-2 py-1">
        <div class="text-xs opacity-70">Save track as route</div>
        <label class="text-xs flex flex-col gap-0.5">
          From
          <input type="datetime-local" class="bg-dark px-1 rounded text-white" bind:value={t0Str} />
        </label>
        <label class="text-xs flex flex-col gap-0.5">
          To
          <input type="datetime-local" class="bg-dark px-1 rounded text-white" bind:value={t1Str} />
        </label>
        <label class="text-xs flex items-center gap-2">
          Granularity
          <input
            type="number"
            min="10"
            step="10"
            class="bg-dark px-1 rounded text-white w-20"
            bind:value={granularity}
          />
          m
        </label>
        {#if fetchTrackWindow}
          <div class="flex items-center gap-2 text-xs">
            <button
              class="px-2 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40"
              onclick={loadWindow}
              disabled={fetching}
              title="Pull the full recorded track for this window from the cloud"
            >
              {fetching ? "Loading…" : "Load full history"}
            </button>
            <span class="opacity-60">
              {windowPoints ? `fetched ${windowPoints.length} pts` : "using loaded track"}
            </span>
          </div>
          {#if fetchError}
            <div class="text-warning-dark text-xs">{fetchError}</div>
          {/if}
        {/if}
        <div class="text-xs opacity-70">
          {trackPreview.inWindow} track points → {trackPreview.waypoints.length} waypoints ({trackPreview.distanceNm.toFixed(
            1
          )} nm)
          {#if trackPreview.capped}<span class="text-warning-dark"> · capped at 500</span>{/if}
        </div>
        <input
          class="bg-dark px-1 rounded text-white"
          placeholder="Route name"
          bind:value={trackName}
        />
        <label class="text-xs flex items-center gap-1.5">
          Color
          <input
            type="color"
            class="w-6 h-5 bg-transparent border border-dark rounded cursor-pointer p-0"
            bind:value={trackColor}
          />
        </label>
        <div class="flex gap-2 text-xs">
          <button
            class="px-2 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40"
            onclick={commitSaveTrack}
            disabled={busy || !trackName.trim() || trackPreview.waypoints.length < 2}>Save</button
          >
          <button
            class="px-2 py-0.5 border border-dark rounded hover:bg-dark"
            onclick={() => (trackFormOpen = false)}>Cancel</button
          >
        </div>
      </div>
    {:else}
      <button
        class="px-2 py-0.5 border border-dark rounded hover:bg-dark disabled:opacity-40 text-xs"
        onclick={openTrackForm}
        disabled={busy}
      >
        Save track as route…
      </button>
    {/if}
  {/if}
</div>
