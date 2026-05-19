/**
 * GFS PRMSL isobar overlay. Fetches a GeoJSON FeatureCollection from
 * /noaa-weather/data/gfs-isobars/latest.json?fh=<fh> (the same cache
 * pipeline the wind/wave layers use, just on the FetchBytes branch in
 * the Go cache so the body is already-serialised GeoJSON) and renders
 * it as an OL Vector layer.
 *
 * The backend emits one short LineString feature per (cell, level)
 * crossing — they aren't stitched into long polylines. That keeps the
 * server side simple and is harmless visually: at 0.25° GFS resolution
 * the segments are ~25 km long and the eye reads them as continuous
 * contours. For labels we rely on OL's declutter so neighbouring
 * segments along the same isobar don't all paint their text on top of
 * each other.
 */
import VectorLayer from "ol/layer/Vector.js";
import VectorSource from "ol/source/Vector.js";
import GeoJSON from "ol/format/GeoJSON.js";
import { Style, Stroke, Text, Fill, Circle as CircleStyle } from "ol/style.js";
import type { Feature } from "ol";
import type { LineString } from "ol/geom.js";

/** Subset of marineMap's mapGlobal that we need. */
export interface IsobarMapHandle {
  layerOptions: Array<{
    name: string;
    displayName?: string;
    on: boolean;
    layer?: any;
    parent?: string;
    minZoom?: number;
    maxZoom?: number;
  }>;
}

export interface IsobarLayerHandle {
  /** Refetch at the given forecast hour and replace the rendered features. */
  setForecastHour(hour: number): Promise<void>;
  /** Current forecast hour. */
  getForecastHour(): number;
  /** Reference time (GFS run) of the currently loaded data, ISO string. */
  getRunTime(): string | null;
  /** Underlying OL layer for direct manipulation. */
  layer: VectorLayer<any>;
}

interface IsobarMeta {
  refTime: string;
  forecastTime: number;
  stepHPa: number;
}

interface IsobarFeatureCollection {
  type: "FeatureCollection";
  features: any[];
  meta?: IsobarMeta;
}

export interface SetupIsobarOptions {
  /** layerOptions name (e.g. "isobars"). */
  layerName: string;
  /** UI label. */
  displayName: string;
  /** Optional parent group (e.g. "weather"). */
  parent?: string;
  /** Backend model identifier (must match a registered Go model name). */
  model: string;
  /** Initial forecast hour. */
  initialForecastHour?: number;
  /** Highest zoom at which the layer is visible. Hidden when zoom > maxZoom. */
  maxZoom?: number;
  /** OL z-index — sit between wind (30) and the base raster. */
  zIndex?: number;
}

export async function setupIsobarLayer(
  mapGlobal: IsobarMapHandle,
  opts: SetupIsobarOptions,
): Promise<IsobarLayerHandle | null> {
  let currentFh = Math.max(0, opts.initialForecastHour ?? 0);
  let currentRunTime: string | null = null;

  const source = new VectorSource({ format: new GeoJSON() });
  const layer = new VectorLayer({
    source,
    // declutter drops overlapping labels, so we can attach a Text
    // style to every feature without the map turning into a noise of
    // overlapping "1012"s along each contour.
    declutter: true,
    style: isobarStyle,
    zIndex: opts.zIndex ?? 28,
  });

  async function loadInto(fh: number): Promise<string | null> {
    try {
      const resp = await fetch(`/noaa-weather/data/${opts.model}/latest.json?fh=${fh}`);
      if (!resp.ok) {
        const body = await resp.text().catch(() => "");
        return body.trim() || `HTTP ${resp.status}`;
      }
      const fc = (await resp.json()) as IsobarFeatureCollection;
      source.clear();
      const feats = source.getFormat()!.readFeatures(fc) as Feature[];
      source.addFeatures(feats);
      currentFh = fh;
      currentRunTime = fc.meta?.refTime ?? null;
      return null;
    } catch (e: any) {
      return String(e?.message ?? e);
    }
  }

  const err = await loadInto(currentFh);
  if (err) {
    console.warn("isobars: initial load failed:", err);
    // We still return a handle so the host can retry on slider scrub —
    // a transient NOMADS hiccup shouldn't disable the layer entirely.
  }

  const existing = mapGlobal.layerOptions.find((l) => l.name === opts.layerName);
  if (existing) {
    existing.layer = layer;
    if (opts.parent !== undefined) existing.parent = opts.parent;
    if (existing.displayName === undefined) existing.displayName = opts.displayName;
    if (opts.maxZoom !== undefined && existing.maxZoom === undefined) {
      existing.maxZoom = opts.maxZoom;
    }
  } else {
    mapGlobal.layerOptions.push({
      name: opts.layerName,
      displayName: opts.displayName,
      on: false,
      layer,
      parent: opts.parent,
      maxZoom: opts.maxZoom,
    });
  }

  return {
    layer,
    getForecastHour: () => currentFh,
    getRunTime: () => currentRunTime,
    async setForecastHour(hour: number) {
      const fh = Math.max(0, Math.round(hour));
      if (fh === currentFh && source.getFeatures().length > 0) return;
      const err = await loadInto(fh);
      if (err) console.warn(`isobars: load fh=${fh} failed:`, err);
    },
  };
}

// --- Style ----------------------------------------------------------------

// Cached Style instances per (hPa, labelText). Building a new Style on
// every feature paint is enough overhead at ~20 k features to make
// scrolls feel sticky; OL only mutates these on resolution change so we
// can share them across paints freely.
const styleCache = new Map<string, Style>();

function isobarStyle(feature: any, resolution: number): Style | undefined {
  const props = feature.getProperties();
  const kind: string | undefined = props.kind;
  const hPa: number = typeof props.hPa === "number" ? props.hPa : 1000;
  // H / L Point features short-circuit the contour path — they always
  // render, with a large red/blue letter plus the field value below.
  if (kind === "H" || kind === "L") {
    return extremumStyle(kind, hPa);
  }
  // Contour-line visibility ladder against the 2 hPa spacing the
  // backend now emits: at far zoom we drop the in-between 2 hPa lines
  // entirely (only the 4 hPa marine-standard contours show); close in
  // every 2 hPa line draws. Labels are sparser: every 8 hPa far,
  // every 4 hPa close. Threshold values are degrees-per-pixel under
  // useGeographic, so smaller resolution = more zoomed in.
  const isHalfStep = hPa % 4 !== 0;
  if (isHalfStep && resolution > 0.3) {
    return undefined; // hide 2 hPa lines at overview zoom
  }
  const labelEvery = resolution > 0.5 ? 8 : 4;
  const showLabel = !isHalfStep && hPa % labelEvery === 0;
  // Only the midpoint of each cell-level segment gets a label so we
  // don't draw text on every short stub. We pick segments where the
  // feature's first coordinate falls on a coarse lon/lat lattice.
  let labelText: string | null = null;
  if (showLabel) {
    const geom = feature.getGeometry() as LineString | null;
    if (geom) {
      const c = geom.getFirstCoordinate();
      // ~5° spacing at far zoom, 1° at close. Picking by lon%step (with
      // tolerance) gives a roughly even sampling that doesn't move
      // around as the user pans.
      const step = resolution > 0.5 ? 5 : 1;
      const lon = c[0];
      const lat = c[1];
      const lonHit = Math.abs(lon - Math.round(lon / step) * step) < step * 0.15;
      const latHit = Math.abs(lat - Math.round(lat / step) * step) < step * 0.15;
      if (lonHit && latHit) {
        labelText = String(hPa);
      }
    }
  }
  const key = `${hPa}:${labelText ?? ""}`;
  const cached = styleCache.get(key);
  if (cached) return cached;
  const s = buildStyle(hPa, labelText, isHalfStep);
  styleCache.set(key, s);
  return s;
}

// buildStyle picks stroke weight/colour from the contour value.
// Visual ladder against the 2 hPa backend spacing:
//   reference (1000 hPa): darkest, 2.0 px
//   heavy (every 20 hPa): mid-grey, 1.4 px
//   standard (every 4 hPa, the canonical NWS analysis): 1.0 px
//   half-step (the new 2 hPa fill): light grey, 0.6 px
// Sub-1000 contours don't get special weight any more — the half-step
// fill already conveys gradient density.
function buildStyle(
  hPa: number,
  labelText: string | null,
  isHalfStep: boolean,
): Style {
  const isReference = hPa === 1000;
  const isHeavy = hPa % 20 === 0;
  let color: string;
  let width: number;
  if (isReference) {
    color = "rgba(40, 40, 40, 0.95)";
    width = 2.0;
  } else if (isHeavy) {
    color = "rgba(50, 50, 50, 0.85)";
    width = 1.4;
  } else if (isHalfStep) {
    color = "rgba(90, 90, 90, 0.55)";
    width = 0.6;
  } else {
    color = "rgba(60, 60, 60, 0.78)";
    width = 1.0;
  }
  const stroke = new Stroke({ color, width });
  const text = labelText
    ? new Text({
        text: labelText,
        font: "11px sans-serif",
        placement: "point",
        fill: new Fill({ color: "#222" }),
        stroke: new Stroke({ color: "rgba(255,255,255,0.85)", width: 3 }),
        overflow: false,
      })
    : undefined;
  return new Style({ stroke, text });
}

// extremumStyle paints the big bold "H" or "L" letter at a pressure
// extremum with the field value (in hPa) on a second line below. Red
// for highs, blue for lows — the surface-analysis convention. Cached
// per (kind, hPa) so OL can swap styles without rebuilding.
function extremumStyle(kind: "H" | "L", hPa: number): Style {
  const key = `extremum:${kind}:${hPa}`;
  const cached = styleCache.get(key);
  if (cached) return cached;
  const color = kind === "H" ? "#b71c1c" : "#0d47a1";
  // Two-line label: glyph on top, value below. OL's Text supports
  // multi-line via \n; offsetY centres the block around the point.
  const s = new Style({
    image: new CircleStyle({
      radius: 0,
      // Zero-radius circle keeps the feature pickable via Text-only
      // styles while leaving no visible mark behind the letter.
      stroke: new Stroke({ color: "rgba(0,0,0,0)", width: 0 }),
    }),
    text: new Text({
      text: `${kind}\n${hPa}`,
      font: "bold 18px sans-serif",
      placement: "point",
      fill: new Fill({ color }),
      stroke: new Stroke({ color: "rgba(255,255,255,0.95)", width: 4 }),
      offsetY: 0,
      overflow: true,
    }),
  });
  styleCache.set(key, s);
  return s;
}
