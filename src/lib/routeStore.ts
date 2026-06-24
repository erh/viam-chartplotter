// Client-side access to saved routes. Storage lives in the Viam location
// metadata, but the browser never talks to the cloud directly — it goes through
// the nav service's routes_* DoCommand verbs (see nav_routes.go), which
// authenticate with the machine's own credentials. So routes work even when the
// browser has no app.viam.com credentials of its own.
//
// The read-modify-write, schema/size guards, foreign-key preservation and stats
// all live on the Go side now; this module just shapes the DoCommand calls and
// the few helpers the UI needs (id/color generation, size warning).

import type { LatLng } from "./simplify";

export type RouteScope = "location" | "parent";

export interface Route {
  id: string;
  name: string;
  notes?: string;
  color?: string;
  source: "manual" | "track";
  createdAt: string;
  updatedAt: string;
  waypoints: LatLng[];
  stats?: { distanceNm: number; count: number };
  // Where this route lives: "location" = this machine's location; "parent" =
  // inherited from an ancestor location (read-only here). Set by the backend on
  // list responses.
  scope?: RouteScope;
}

// Minimal nav-service surface: a DoCommand passthrough. Backed by
// VIAM.NavigationClient(...).doCommand in the app; a fake in tests.
export interface RoutesApi {
  doCommand(cmd: Record<string, unknown>): Promise<Record<string, unknown>>;
}

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

// Generate a stable, unique-ish route id (browser runtime).
export function newRouteId(): string {
  const time = Date.now().toString(36);
  let rand = "";
  if (typeof crypto !== "undefined" && crypto.getRandomValues) {
    const buf = new Uint8Array(2);
    crypto.getRandomValues(buf);
    rand = Array.from(buf, (b) => b.toString(16).padStart(2, "0")).join("");
  } else {
    rand = Math.floor(Math.random() * 0xffff)
      .toString(16)
      .padStart(4, "0");
  }
  return `rte_${time}_${rand}`;
}

export function nextColor(existing: Route[]): string {
  const used = new Set(existing.map((r) => r.color).filter(Boolean));
  return PALETTE.find((c) => !used.has(c)) ?? PALETTE[existing.length % PALETTE.length];
}

export function sizeWarning(routes: Route[]): boolean {
  return new TextEncoder().encode(JSON.stringify(routes)).length > SIZE_WARN_BYTES;
}

export async function listRoutes(api: RoutesApi): Promise<Route[]> {
  const resp = await api.doCommand({ routes_list: true });
  const routes = resp?.routes;
  return Array.isArray(routes) ? (routes as Route[]) : [];
}

export async function saveRoute(api: RoutesApi, route: Route): Promise<void> {
  await api.doCommand({ routes_save: { route } });
}

export async function deleteRoute(api: RoutesApi, id: string): Promise<void> {
  await api.doCommand({ routes_delete: { id } });
}

export async function renameRoute(
  api: RoutesApi,
  id: string,
  fields: { name?: string; notes?: string; color?: string },
  nowISO: string
): Promise<void> {
  await api.doCommand({ routes_rename: { id, ...fields, updatedAt: nowISO } });
}
