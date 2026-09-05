// Auto-routing client. The planner lives on the chart server (Go:
// render/autoroute.go) because that is where the ENC depth data is; the
// browser hands it two points and gets back a waypoint list.
//
// The safe depth is the hard constraint — the route never crosses water
// charted shoaler than it — and the ideal depth is the soft one: among safe
// routes, prefer the one that stays that deep. Leave either blank and the
// server uses the boat's configured draft.

import type { LatLng } from "./simplify";

export interface AutoRouteRequest {
  start: LatLng;
  end: LatLng;
  /** Hard minimum depth in feet. Omitted = the module's configured draft. */
  safeDepthFt?: number;
  /** Preferred depth in feet. Omitted = the module's ideal_depth, else 2x draft. */
  idealDepthFt?: number;
  /** No-go buffer off land and shoals, metres. */
  clearanceM?: number;
  /** Charted area classes to steer around. Extensible server-side. */
  avoid?: "restricted"[];
  maxWaypoints?: number;
}

export interface AutoRouteResult {
  waypoints: LatLng[];
  distance_meters: number;
  direct_meters: number;
  /** Shoalest charted depth on the route; null when none of it was charted. */
  min_depth_meters: number | null;
  crossed_unknown: boolean;
  safe_depth_meters: number;
  ideal_depth_meters: number;
  snapped_start: boolean;
  snapped_end: boolean;
  cell_size_meters: number;
  /**
   * How many independently planned runs the route was split into. More than 1
   * means it was too long to plan whole at a useful resolution, and
   * cell_size_meters is the coarsest any section used.
   */
  sections: number;
  warnings?: string[];
}

const METRES_TO_FEET = 3.28084;
const NM = 1852;

export function metresToFeet(m: number): number {
  return m * METRES_TO_FEET;
}

export function metresToNm(m: number): number {
  return m / NM;
}

/** Builds the /noaa-enc/autoroute query. Exported for testing. */
export function autoRouteUrl(base: string, req: AutoRouteRequest): string {
  const p = new URLSearchParams({
    startLat: String(req.start.lat),
    startLon: String(req.start.lng),
    endLat: String(req.end.lat),
    endLon: String(req.end.lng),
  });
  // Only send what the operator actually chose — an omitted parameter means
  // "use the boat's configured value", which is not the same as sending 0.
  if (req.safeDepthFt != null && req.safeDepthFt > 0) p.set("sd", String(req.safeDepthFt));
  if (req.idealDepthFt != null && req.idealDepthFt > 0) p.set("ideal", String(req.idealDepthFt));
  if (req.clearanceM != null && req.clearanceM >= 0) p.set("clearance", String(req.clearanceM));
  if (req.maxWaypoints != null && req.maxWaypoints > 1)
    p.set("max_waypoints", String(req.maxWaypoints));
  if (req.avoid?.length) p.set("avoid", req.avoid.join(","));
  return `${base}/noaa-enc/autoroute?${p.toString()}`;
}

// The chart server may be a different origin from the app (a split
// deployment); /app-config says which. Resolved once and cached — it can't
// change without a page reload.
let tileBasePromise: Promise<string> | null = null;
const HOSTED_TILE_FALLBACK = "https://nycmaps.checkmatemaps.com";

export function resolveTileBase(): Promise<string> {
  if (!tileBasePromise) {
    tileBasePromise = (async () => {
      try {
        const resp = await fetch("/app-config");
        if (!resp.ok) return HOSTED_TILE_FALLBACK;
        const cfg = await resp.json();
        if (cfg && typeof cfg.tileServerBaseURL === "string") {
          return cfg.tileServerBaseURL.replace(/\/$/, "");
        }
      } catch {
        return HOSTED_TILE_FALLBACK;
      }
      return "";
    })();
  }
  return tileBasePromise;
}

export interface OptimizeRequest {
  waypoints: LatLng[];
  safeDepthFt?: number;
  idealDepthFt?: number;
  clearanceM?: number;
  avoid?: "restricted"[];
  maxWaypoints?: number;
  /**
   * Keep every waypoint the operator placed, re-planning only the water
   * between them (the default). False lets the smoother straighten through
   * them and drop the redundant ones.
   */
  keepWaypoints?: boolean;
}

/**
 * Re-plans an existing waypoint list around land, shoals and obstructions.
 * `direct_meters` in the result is the ORIGINAL route's length, so the caller
 * can show what the optimisation cost or saved.
 */
export async function optimizeRoute(req: OptimizeRequest): Promise<AutoRouteResult> {
  const base = await resolveTileBase();
  const resp = await fetch(`${base}/noaa-enc/optimize`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      waypoints: req.waypoints.map((w) => ({ lat: w.lat, lng: w.lng })),
      // Only send what was actually chosen — an omitted field means "use the
      // boat's configured value", which is not the same as sending 0.
      ...(req.safeDepthFt != null && req.safeDepthFt > 0 ? { safe_depth_ft: req.safeDepthFt } : {}),
      ...(req.idealDepthFt != null && req.idealDepthFt > 0
        ? { ideal_depth_ft: req.idealDepthFt }
        : {}),
      ...(req.clearanceM != null && req.clearanceM >= 0 ? { clearance_m: req.clearanceM } : {}),
      ...(req.maxWaypoints != null && req.maxWaypoints > 1
        ? { max_waypoints: req.maxWaypoints }
        : {}),
      ...(req.keepWaypoints != null ? { keep_waypoints: req.keepWaypoints } : {}),
      ...(req.avoid?.length ? { avoid: req.avoid } : {}),
    }),
  });
  if (!resp.ok) {
    let msg = `optimize failed (${resp.status})`;
    try {
      const body = await resp.json();
      if (body && typeof body.error === "string") msg = body.error;
    } catch {
      // non-JSON error body; keep the status message
    }
    throw new Error(msg);
  }
  return (await resp.json()) as AutoRouteResult;
}

/**
 * Plans a route between two points. Throws with the server's own message on
 * failure — "no safe route found …" is the common one and is worth showing
 * verbatim, since it names the depth that made it impossible.
 */
export async function planAutoRoute(req: AutoRouteRequest): Promise<AutoRouteResult> {
  const base = await resolveTileBase();
  const resp = await fetch(autoRouteUrl(base, req));
  if (!resp.ok) {
    let msg = `auto-route failed (${resp.status})`;
    try {
      const body = await resp.json();
      if (body && typeof body.error === "string") msg = body.error;
    } catch {
      // non-JSON error body; keep the status message
    }
    throw new Error(msg);
  }
  return (await resp.json()) as AutoRouteResult;
}

/**
 * The human-readable cautions to show beside a planned route: the server's own
 * warnings plus the two derived from the result that a skipper should see
 * before loading it.
 */
export function routeCautions(res: AutoRouteResult): string[] {
  const out = [...(res.warnings ?? [])];
  if (res.crossed_unknown) {
    out.push("part of this route crosses water with no charted depth");
  }
  if (res.sections > 1) {
    out.push(
      `too long to plan in one piece — split into ${res.sections} sections, coarsest grid ${Math.round(res.cell_size_meters)} m`
    );
  }
  if (res.min_depth_meters != null && Number.isFinite(res.min_depth_meters)) {
    out.push(
      `shoalest charted depth on the route: ${metresToFeet(res.min_depth_meters).toFixed(1)} ft`
    );
  }
  return out;
}
