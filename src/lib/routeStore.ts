// Client-side access to saved routes. Storage lives in the Viam location
// metadata, but the browser never talks to the cloud directly — it goes through
// the nav service (see nav_routes.go), which authenticates with the machine's
// own credentials. So routes work even when the browser has no app.viam.com
// credentials of its own.
//
// Names and waypoints go through the navigation API's Route RPCs. The API's
// Route has no slot for the library's other fields (id, notes, color, source,
// dates, stats, scope), so those come back from the routes_meta DoCommand and
// are written through the RPCs' extra. Every mutation addresses a route by id,
// which the service accepts wherever the API expects a route name.
//
// The read-modify-write, schema/size guards, foreign-key preservation and stats
// all live on the Go side; this module just shapes the calls and the few
// helpers the UI needs (color choice, size warning).

import type { JsonValue, Route as ApiRoute, Waypoint } from "@viamrobotics/sdk";
import { latLngWaypoint, waypointLatLng } from "./navRoute";
import type { LatLng } from "./simplify";

export type RouteScope = "location" | "parent";

export interface Route {
  id: string;
  name: string;
  notes?: string;
  color?: string;
  // "auto" is a route the server's ENC auto-router planned (see autoRoute.ts).
  source: "manual" | "track" | "auto";
  createdAt: string;
  updatedAt: string;
  waypoints: LatLng[];
  stats?: { distanceNm: number; count: number };
  // Where this route lives: "location" = this machine's location; "parent" =
  // inherited from an ancestor location (read-only here). Set by the backend on
  // list responses.
  scope?: RouteScope;
}

// The route calls of the nav service. NavigationClient satisfies this; tests
// use a fake.
export interface RoutesApi {
  listRoutes(): Promise<ApiRoute[]>;
  addRoute(
    routeName: string,
    waypoints: Waypoint[],
    extra?: Record<string, JsonValue>
  ): Promise<unknown>;
  updateRoute(
    routeName: string,
    newName?: string,
    waypoints?: Waypoint[],
    extra?: Record<string, JsonValue>
  ): Promise<unknown>;
  deleteRoute(routeName: string): Promise<void>;
  doCommand(command: Record<string, JsonValue>): Promise<JsonValue>;
}

/** A route's library fields as routes_meta reports them (everything but waypoints). */
type RouteMeta = Omit<Route, "waypoints">;

// Soft client-side warning threshold (the hard limit is enforced by the
// backend). Kept here so the panel can warn before a save is rejected.
const SIZE_WARN_BYTES = 200 * 1024;

const PALETTE = [
  "#ff8800",
  "#1e90ff",
  "#2ecc71",
  "#e74c3c",
  "#9b59b6",
  "#f1c40f",
  "#16a085",
  "#e84393",
];

export function nextColor(existing: Route[]): string {
  const used = new Set(existing.map((r) => r.color).filter(Boolean));
  return PALETTE.find((c) => !used.has(c)) ?? PALETTE[existing.length % PALETTE.length];
}

export function sizeWarning(routes: Route[]): boolean {
  return new TextEncoder().encode(JSON.stringify(routes)).length > SIZE_WARN_BYTES;
}

/**
 * Pair ListRoutes results with routes_meta entries. Both come from the same
 * server-side list, so they normally line up by index; if they don't (a route
 * changed between the two calls), fall back to the first unused entry with the
 * same name. A route with no metadata is dropped — it has no id to edit by.
 */
export function pairRoutes(apiRoutes: ApiRoute[], metas: RouteMeta[]): Route[] {
  const used = new Set<number>();
  const out: Route[] = [];
  apiRoutes.forEach((r, i) => {
    let j = metas[i]?.name === r.name && !used.has(i) ? i : -1;
    if (j < 0) j = metas.findIndex((m, k) => m.name === r.name && !used.has(k));
    if (j < 0) return;
    used.add(j);
    const waypoints = r.waypoints.map(waypointLatLng).filter((p) => p !== null);
    out.push({ ...metas[j], waypoints });
  });
  return out;
}

export async function listRoutes(api: RoutesApi): Promise<Route[]> {
  const [apiRoutes, resp] = await Promise.all([
    api.listRoutes(),
    api.doCommand({ routes_meta: true }),
  ]);
  const metas = (resp as { routes?: unknown } | null)?.routes;
  return pairRoutes(apiRoutes, Array.isArray(metas) ? (metas as RouteMeta[]) : []);
}

/** Save a new route; the service assigns its id and timestamps. */
export async function createRoute(
  api: RoutesApi,
  route: {
    name: string;
    source: Route["source"];
    color?: string;
    notes?: string;
    waypoints: LatLng[];
  }
): Promise<void> {
  const extra: Record<string, JsonValue> = { source: route.source };
  if (route.color !== undefined) extra.color = route.color;
  if (route.notes !== undefined) extra.notes = route.notes;
  await api.addRoute(route.name, route.waypoints.map(latLngWaypoint), extra);
}

/** Overwrite a route's geometry, keeping everything else. */
export async function replaceRouteWaypoints(
  api: RoutesApi,
  id: string,
  waypoints: LatLng[]
): Promise<void> {
  await api.updateRoute(id, undefined, waypoints.map(latLngWaypoint));
}

/** Change a route's name, notes and/or color. */
export async function updateRoute(
  api: RoutesApi,
  id: string,
  fields: { name?: string; notes?: string; color?: string }
): Promise<void> {
  const extra: Record<string, JsonValue> = {};
  if (fields.notes !== undefined) extra.notes = fields.notes;
  if (fields.color !== undefined) extra.color = fields.color;
  await api.updateRoute(id, fields.name, undefined, extra);
}

export async function deleteRoute(api: RoutesApi, id: string): Promise<void> {
  await api.deleteRoute(id);
}
