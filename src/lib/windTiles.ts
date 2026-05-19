/**
 * Tile-aware fetcher for the wind-publisher's R2/CDN output.
 *
 * Background: when a chartplotter fleet scales past ~100 instances, having
 * every chartplotter pull from ECMWF Open Data crosses their fair-use
 * threshold. The wind-publisher (a separate Viam component, see
 * wind_publisher_resource.go) fetches once per cycle, splits each forecast
 * hour into a 6×6 tile grid, gzips each tile, and uploads to Cloudflare
 * R2. Chartplotters then fetch from that public bucket — same JSON shape
 * the local /noaa-weather/data/ecmwf endpoint produces, just per-tile and
 * cropped to the requested region.
 *
 * This module provides:
 *
 *   - Tile-grid math that mirrors `wind_publisher_tiler.go` exactly. The
 *     server publishes 36 tiles; the browser picks the right one from its
 *     viewport bbox without any server round-trip.
 *   - A `WindCDNClient` that resolves the latest cycle (60s cache), maps a
 *     viewport bbox + fh to a tile URL, and falls back to the legacy local
 *     endpoint on any CDN error so a CDN outage doesn't kill wind display.
 *
 * The tile-grid constants here MUST match the server's
 * wind_publisher_tiler.go constants. Both are surfaced via the server's
 * /noaa-weather/config endpoint so a future grid change updates the
 * frontend automatically — but for now they're hard-coded as a defensive
 * default the constructor overrides when the config fetch lands.
 */

export interface TileGridConfig {
  cols: number;        // longitude bands
  rows: number;        // latitude bands
  overlapDeg: number;  // margin published past each nominal edge
  nominalLonW: number; // west edge of the grid origin (≡ tileNominalLonW server-side)
  nominalLatS: number; // south edge of the grid origin
}

export const DEFAULT_TILE_GRID: TileGridConfig = {
  cols: 6,
  rows: 6,
  overlapDeg: 10,
  nominalLonW: -180,
  nominalLatS: -90,
};

export interface TileManifest {
  key: string;
  /** [w, s, e, n] — the no-overlap band used for centre lookup. */
  nominalBbox: [number, number, number, number];
  /** [w, s, e, n] — what the data file actually covers. */
  publishedBbox: [number, number, number, number];
}

export interface LatestPointer {
  model: string;
  cycle: string;          // "20060102T15"
  publishedAt: string;
  fhs: number[];
  tiles: TileManifest[];
  previousCycles?: string[];
}

/** Map a longitude into [-180, 180). Mirrors the Go `wrapLon`. */
function wrapLon(lon: number): number {
  while (lon < -180) lon += 360;
  while (lon >= 180) lon -= 360;
  return lon;
}

/** Inner bbox fully contained in outer (closed intervals). */
function bboxFitsInside(
  inner: [number, number, number, number],
  outer: [number, number, number, number],
): boolean {
  return (
    inner[0] >= outer[0] &&
    inner[1] >= outer[1] &&
    inner[2] <= outer[2] &&
    inner[3] <= outer[3]
  );
}

/**
 * Build the slug for a tile whose nominal SW corner is at (lonW, latS).
 * Mirrors the Go `tileKey` byte-for-byte so a frontend-computed key
 * matches the key the publisher used at upload time.
 */
function tileKey(lonW: number, latS: number): string {
  const lonHemi = lonW < 0 ? "W" : "E";
  const lonAbs = Math.abs(lonW);
  const latHemi = latS < 0 ? "S" : "N";
  const latAbs = Math.abs(latS);
  // Match Go's %g format: integers print without trailing ".0".
  const fmt = (n: number) => (Number.isInteger(n) ? n.toString() : n.toString());
  return `lon${lonHemi}${fmt(lonAbs)}_lat${latHemi}${fmt(latAbs)}`;
}

/**
 * For a viewport bbox `[w, s, e, n]`, return the tile that contains
 * its centre. The second return is `false` when the viewport is wider
 * than one tile's published extent (the caller should stitch, or fall
 * back to the legacy global endpoint for very-zoomed-out views).
 */
export function tileForBbox(
  viewport: [number, number, number, number],
  grid: TileGridConfig = DEFAULT_TILE_GRID,
): { tile: TileManifest; fits: boolean } {
  let cx = (viewport[0] + viewport[2]) / 2;
  let cy = (viewport[1] + viewport[3]) / 2;
  cx = wrapLon(cx);
  if (cy < -90) cy = -90;
  if (cy > 90) cy = 90;
  const lonStep = 360 / grid.cols;
  const latStep = 180 / grid.rows;
  let col = Math.floor((cx - grid.nominalLonW) / lonStep);
  let row = Math.floor((cy - grid.nominalLatS) / latStep);
  if (col < 0) col = 0;
  else if (col >= grid.cols) col = grid.cols - 1;
  if (row < 0) row = 0;
  else if (row >= grid.rows) row = grid.rows - 1;

  const lonW = grid.nominalLonW + col * lonStep;
  const latS = grid.nominalLatS + row * latStep;
  const lonE = lonW + lonStep;
  const latN = latS + latStep;
  const pubS = Math.max(-90, latS - grid.overlapDeg);
  const pubN = Math.min(90, latN + grid.overlapDeg);
  const tile: TileManifest = {
    key: tileKey(lonW, latS),
    nominalBbox: [lonW, latS, lonE, latN],
    publishedBbox: [lonW - grid.overlapDeg, pubS, lonE + grid.overlapDeg, pubN],
  };
  return { tile, fits: bboxFitsInside(viewport, tile.publishedBbox) };
}

/** Server response shape for /noaa-weather/config. */
export interface ServerWindConfig {
  windCDNBaseURL: string;
  tileGrid: TileGridConfig;
}

/**
 * Client for the wind-publisher's R2/CDN output. Construct one per
 * model. Fetches latest.json lazily, caches it for 60 s, then resolves
 * (viewport, fh) → URL strings the existing windLayer.ts fetcher can
 * use.
 *
 * Caller is responsible for catching CDN errors and falling back to
 * the local endpoint — this client doesn't proxy that. Keeps the
 * failure surface explicit.
 */
export class WindCDNClient {
  private latest: LatestPointer | null = null;
  private latestFetchedAt = 0;
  private inflightLatest: Promise<LatestPointer> | null = null;
  private readonly cdnBase: string;
  private readonly model: string;
  private readonly grid: TileGridConfig;

  constructor(cdnBase: string, model: string, grid: TileGridConfig = DEFAULT_TILE_GRID) {
    // Trim trailing slash for predictable URL joining. Same shape the
    // server stores in WeatherCache.windCDNBaseURL.
    this.cdnBase = cdnBase.replace(/\/+$/, "");
    this.model = model;
    this.grid = grid;
  }

  /** Has the CDN been configured? Cheap probe so callers can skip
   *  tile fetching entirely when running against a single-instance
   *  dev module that publishes nothing. */
  isConfigured(): boolean {
    return this.cdnBase !== "";
  }

  /** Returns the URL clients should hit for a (viewport, fh) pair.
   *  Resolves the current cycle if it hasn't been fetched within the
   *  last minute, then composes the immutable tile URL. */
  async urlFor(viewport: [number, number, number, number], fh: number): Promise<string> {
    const latest = await this.fetchLatest();
    const { tile } = tileForBbox(viewport, this.grid);
    // Path mirrors R2UploaderUploadCycle's key format:
    //   wind/<model>/<cycle>/f<fh>/<tile>.json.gz
    const fhStr = `f${String(fh).padStart(3, "0")}`;
    return `${this.cdnBase}/wind/${this.model}/${latest.cycle}/${fhStr}/${tile.key}.json.gz`;
  }

  /** Force-refresh the latest pointer (e.g. after a publish event). */
  invalidate(): void {
    this.latest = null;
    this.latestFetchedAt = 0;
  }

  private async fetchLatest(): Promise<LatestPointer> {
    const now = Date.now();
    if (this.latest && now - this.latestFetchedAt < 60_000) {
      return this.latest;
    }
    // Coalesce concurrent fetchers — the layer's prefetcher can fire
    // 4 fhs in parallel on first load, no point hitting R2 four times
    // for the same pointer.
    if (this.inflightLatest) return this.inflightLatest;
    const url = `${this.cdnBase}/wind/${this.model}/latest.json`;
    this.inflightLatest = fetch(url, { cache: "no-store" })
      .then(async (resp) => {
        if (!resp.ok) throw new Error(`latest.json: HTTP ${resp.status}`);
        const body = (await resp.json()) as LatestPointer;
        this.latest = body;
        this.latestFetchedAt = Date.now();
        return body;
      })
      .finally(() => {
        this.inflightLatest = null;
      });
    return this.inflightLatest;
  }
}

/** Helper to fetch and parse the small /noaa-weather/config endpoint.
 *  Returns null if the request fails — callers should fall back to
 *  legacy local-fetch behaviour in that case. */
export async function loadServerWindConfig(): Promise<ServerWindConfig | null> {
  try {
    const resp = await fetch("/noaa-weather/config");
    if (!resp.ok) return null;
    return (await resp.json()) as ServerWindConfig;
  } catch {
    return null;
  }
}
