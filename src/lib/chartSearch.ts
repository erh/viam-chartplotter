// Chart search client. Finds anything named in the NOAA ENC store — lights,
// wrecks, canyons, channels, anchorages, harbours — and gives back somewhere
// to look on the map.
//
// Search runs on the chart server (it owns the feature store); this module
// shapes the request, debounces the type-ahead, and works out how the map
// should frame a hit.

import { resolveTileBase } from "./autoRoute";

export interface SearchHit {
  name: string;
  /** S-57 object class acronym, e.g. "LIGHTS". */
  class: string;
  /** That class in words ("Light"), or the acronym when we have no wording. */
  label: string;
  cell: string;
  lat: number;
  lng: number;
  /** [minLon, minLat, maxLon, maxLat] — a feature's full extent. */
  bbox: [number, number, number, number];
  /** Metres from the origin passed to the search, or -1 when none was. */
  distance_meters: number;
}

/** Builds the /noaa-enc/search query. Exported for testing. */
export function searchUrl(
  base: string,
  q: string,
  opts: { origin?: { lat: number; lng: number } | null; limit?: number; objectClass?: string } = {}
): string {
  const p = new URLSearchParams({ q });
  if (opts.origin) {
    // Nearest-first only makes sense with somewhere to measure from. Without
    // it the server falls back to alphabetical.
    p.set("lat", String(opts.origin.lat));
    p.set("lon", String(opts.origin.lng));
  }
  if (opts.limit != null && opts.limit > 0) p.set("limit", String(opts.limit));
  if (opts.objectClass) p.set("class", opts.objectClass);
  return `${base}/noaa-enc/search?${p.toString()}`;
}

export interface SearchResponse {
  hits: SearchHit[];
  /**
   * The terms that actually matched. When it differs from what was typed, the
   * full phrase found nothing and this is a narrower answer — say so rather
   * than present it as an exact match.
   */
  matchedQuery: string;
}

export async function searchChart(
  q: string,
  opts: { origin?: { lat: number; lng: number } | null; limit?: number; objectClass?: string } = {}
): Promise<SearchResponse> {
  const trimmed = q.trim();
  if (!trimmed) return { hits: [], matchedQuery: trimmed };
  const base = await resolveTileBase();
  const resp = await fetch(searchUrl(base, trimmed, opts));
  if (!resp.ok) {
    let msg = `search failed (${resp.status})`;
    try {
      const body = await resp.json();
      if (body && typeof body.error === "string") msg = body.error;
    } catch {
      // non-JSON error body; keep the status message
    }
    throw new Error(msg);
  }
  const body = await resp.json();
  return {
    hits: (body?.results ?? []) as SearchHit[],
    matchedQuery: typeof body?.matched_query === "string" ? body.matched_query : trimmed,
  };
}

/** Shortest query worth sending. One or two letters match half the chart. */
export const MIN_QUERY_LENGTH = 3;

/**
 * How the map should frame a hit. A point feature (a buoy, a light) has no
 * extent, so centre on it at a close zoom; an area (a canyon, a channel) does,
 * so fit the whole thing.
 */
export function framingFor(hit: SearchHit): { kind: "center"; zoom: number } | { kind: "fit" } {
  const [minLon, minLat, maxLon, maxLat] = hit.bbox;
  const spanLon = Math.abs(maxLon - minLon);
  const spanLat = Math.abs(maxLat - minLat);
  // Under ~50 m across in either axis there is nothing to fit to — fitting a
  // degenerate extent zooms to maximum and shows a blank tile.
  const DEGENERATE_DEG = 0.0005;
  if (spanLon < DEGENERATE_DEG && spanLat < DEGENERATE_DEG) {
    return { kind: "center", zoom: 15 };
  }
  return { kind: "fit" };
}

/** Formats a hit's distance for the result row; empty when unknown. */
export function formatDistance(meters: number): string {
  if (!(meters >= 0)) return "";
  const nm = meters / 1852;
  return nm < 10 ? `${nm.toFixed(1)} nm` : `${Math.round(nm)} nm`;
}

/**
 * Debounces an async search so typing doesn't fire a request per keystroke,
 * and so a slow earlier response can never overwrite a newer one.
 */
export function makeSearchRunner<T>(
  run: (q: string) => Promise<T>,
  /** What to report for a query too short to send. */
  empty: T,
  delayMs = 250
): {
  search: (
    q: string,
    onResult: (result: T, q: string) => void,
    onError: (e: Error) => void
  ) => void;
  cancel: () => void;
} {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let seq = 0;

  return {
    search(q, onResult, onError) {
      if (timer) clearTimeout(timer);
      const mine = ++seq; // stamp: a stale response is dropped, not rendered
      if (q.trim().length < MIN_QUERY_LENGTH) {
        onResult(empty, q);
        return;
      }
      timer = setTimeout(() => {
        run(q).then(
          (result) => {
            if (mine === seq) onResult(result, q);
          },
          (e) => {
            if (mine === seq) onError(e instanceof Error ? e : new Error(String(e)));
          }
        );
      }, delayMs);
    },
    cancel() {
      if (timer) clearTimeout(timer);
      seq++; // invalidate anything in flight
    },
  };
}
