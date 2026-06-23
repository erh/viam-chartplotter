// Pure geometry helpers for turning a recorded track into a route.
//
// A track is a dense, time-ordered list of fixes; a route is a handful of
// waypoints. We reduce in two passes (per ROUTES_SPEC.md):
//   1. distance decimation — drop any fix closer than `granularityMeters` to
//      the last kept fix, giving predictable spacing and killing GPS jitter;
//   2. Douglas–Peucker — drop fixes that lie within `toleranceMeters` of the
//      straight line between their neighbours, preserving turns.
// First and last fixes are always kept. All distances are real meters
// (haversine for spacing, a local equirectangular projection for the DP
// perpendicular distance), so the thresholds mean the same thing everywhere.

export interface LatLng {
  lat: number;
  lng: number;
}

export interface SimplifyOptions {
  granularityMeters: number;
  // Defaults to granularityMeters / 2.
  toleranceMeters?: number;
  // Hard ceiling on output size; if DP still exceeds it, tolerance is raised
  // and DP re-run until the count fits (capped=true in the result).
  maxPoints?: number;
}

export interface SimplifiedTrack {
  waypoints: LatLng[];
  // True if maxPoints forced extra simplification beyond the requested
  // tolerance — the UI should surface this so the user can raise granularity.
  capped: boolean;
  inputCount: number;
}

const EARTH_RADIUS_M = 6371008.8;
const DEG2RAD = Math.PI / 180;

// Great-circle distance in meters.
export function haversineMeters(a: LatLng, b: LatLng): number {
  const dLat = (b.lat - a.lat) * DEG2RAD;
  const dLng = (b.lng - a.lng) * DEG2RAD;
  const lat1 = a.lat * DEG2RAD;
  const lat2 = b.lat * DEG2RAD;
  const h = Math.sin(dLat / 2) ** 2 + Math.cos(lat1) * Math.cos(lat2) * Math.sin(dLng / 2) ** 2;
  return 2 * EARTH_RADIUS_M * Math.asin(Math.min(1, Math.sqrt(h)));
}

// Total length of an ordered polyline, in meters.
export function pathLengthMeters(points: LatLng[]): number {
  let total = 0;
  for (let i = 1; i < points.length; i++) {
    total += haversineMeters(points[i - 1], points[i]);
  }
  return total;
}

// Perpendicular distance (meters) from p to the segment a–b, using a local
// equirectangular projection centred on `a` (so x is scaled by cos(lat) and the
// result is in meters). Degenerate segment (a==b) falls back to point distance.
function perpendicularMeters(p: LatLng, a: LatLng, b: LatLng): number {
  const cosLat = Math.cos(a.lat * DEG2RAD);
  const ax = 0;
  const ay = 0;
  const bx = (b.lng - a.lng) * DEG2RAD * cosLat * EARTH_RADIUS_M;
  const by = (b.lat - a.lat) * DEG2RAD * EARTH_RADIUS_M;
  const px = (p.lng - a.lng) * DEG2RAD * cosLat * EARTH_RADIUS_M;
  const py = (p.lat - a.lat) * DEG2RAD * EARTH_RADIUS_M;

  const dx = bx - ax;
  const dy = by - ay;
  const segLenSq = dx * dx + dy * dy;
  if (segLenSq === 0) {
    return Math.hypot(px - ax, py - ay);
  }
  // Project P onto the (clamped) segment, then measure the gap.
  let t = ((px - ax) * dx + (py - ay) * dy) / segLenSq;
  t = Math.max(0, Math.min(1, t));
  const cx = ax + t * dx;
  const cy = ay + t * dy;
  return Math.hypot(px - cx, py - cy);
}

// Distance decimation: keep the first fix, then each fix at least
// granularityMeters from the last kept one. The final fix is always kept so the
// route ends where the track ends.
export function decimateByDistance(points: LatLng[], granularityMeters: number): LatLng[] {
  if (points.length <= 2 || granularityMeters <= 0) {
    return points.slice();
  }
  const out: LatLng[] = [points[0]];
  let last = points[0];
  for (let i = 1; i < points.length - 1; i++) {
    if (haversineMeters(points[i], last) >= granularityMeters) {
      out.push(points[i]);
      last = points[i];
    }
  }
  out.push(points[points.length - 1]);
  return out;
}

// Iterative Douglas–Peucker (explicit stack to avoid deep recursion on long
// tracks). Returns the kept points in order, always including the endpoints.
export function douglasPeucker(points: LatLng[], toleranceMeters: number): LatLng[] {
  const n = points.length;
  if (n <= 2 || toleranceMeters <= 0) {
    return points.slice();
  }
  const keep = new Array<boolean>(n).fill(false);
  keep[0] = true;
  keep[n - 1] = true;
  const stack: Array<[number, number]> = [[0, n - 1]];
  while (stack.length > 0) {
    const [start, end] = stack.pop()!;
    let maxDist = -1;
    let idx = -1;
    for (let i = start + 1; i < end; i++) {
      const d = perpendicularMeters(points[i], points[start], points[end]);
      if (d > maxDist) {
        maxDist = d;
        idx = i;
      }
    }
    if (maxDist > toleranceMeters && idx !== -1) {
      keep[idx] = true;
      stack.push([start, idx]);
      stack.push([idx, end]);
    }
  }
  const out: LatLng[] = [];
  for (let i = 0; i < n; i++) {
    if (keep[i]) out.push(points[i]);
  }
  return out;
}

// Full pipeline: decimate, then Douglas–Peucker, then enforce maxPoints by
// escalating tolerance. Coordinates are normalised to {lat,lng} only (any time
// field on the input is dropped — a route has no timestamps).
export function simplifyTrack(
  points: Array<LatLng & { t?: number }>,
  opts: SimplifyOptions
): SimplifiedTrack {
  const cleaned = points
    .filter(
      (p) =>
        Number.isFinite(p.lat) &&
        Number.isFinite(p.lng) &&
        p.lat >= -90 &&
        p.lat <= 90 &&
        p.lng >= -180 &&
        p.lng <= 180
    )
    .map((p) => ({ lat: p.lat, lng: p.lng }));

  const inputCount = cleaned.length;
  if (inputCount <= 2) {
    return { waypoints: cleaned, capped: false, inputCount };
  }

  const decimated = decimateByDistance(cleaned, opts.granularityMeters);
  let tolerance = opts.toleranceMeters ?? Math.max(1, opts.granularityMeters / 2);
  let result = douglasPeucker(decimated, tolerance);

  let capped = false;
  const maxPoints = opts.maxPoints ?? 0;
  if (maxPoints > 0) {
    // Escalate tolerance geometrically until the route fits. Bounded loop:
    // tolerance grows ×1.6 each pass, so it converges in a handful of steps.
    let guard = 0;
    while (result.length > maxPoints && guard < 40) {
      capped = true;
      tolerance *= 1.6;
      result = douglasPeucker(decimated, tolerance);
      guard++;
    }
  }

  return { waypoints: result, capped, inputCount };
}
