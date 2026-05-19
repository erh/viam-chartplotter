/**
 * Weather overlay using sakitam-fdd/wind-layer's ol-wind build. Wires
 * one ol-wind layer per dataset (wind / waves) — both share the same
 * particle-flow visualisation, just with different colour scales and
 * data semantics.
 *
 * Self-contained: dynamic-imports `ol-wind`, fetches GFS-shape JSON
 * from the bundled `/noaa-weather/...` endpoints, registers each layer
 * in `mapGlobal.layerOptions` (off by default), and patches ol-wind's
 * projection methods so they work under our map's `useGeographic()`
 * mode (where the user projection is EPSG:4326 and ol-wind's built-in
 * `toUserCoordinate` + `transform` double-converts every coordinate
 * back to ~(0, 0)).
 */
import type View from "ol/View";
import type BaseLayer from "ol/layer/Base";
import { transform as projTransform } from "ol/proj.js";
import { apply as applyTransform } from "ol/transform.js";

/** Subset of marineMap's mapGlobal that we need. */
export interface WindMapHandle {
  view: View | null;
  layerOptions: Array<{
    name: string;
    displayName?: string;
    on: boolean;
    layer?: any;
    parent?: string;
    minZoom?: number;
  }>;
}

/** Handle returned by setupWeatherLayer so the host can drive the layer. */
export interface WeatherLayerHandle {
  /** Refetch data at the given forecast hour (snapped to the model's step). */
  setForecastHour(hour: number): Promise<void>;
  /** Current forecast hour (in hours from the model run time). */
  getForecastHour(): number;
  /** Switch to a different upstream model (e.g. "gfs" → "hrrr"). Refetches
   *  at the current forecast hour and updates the underlying field. Returns
   *  null on success, or an error string if the upstream rejected the
   *  request (e.g. disabled stub) so the picker can show *why*. */
  setModel(modelName: string): Promise<string | null>;
  /** Current model identifier. */
  getModel(): string;
  /** Run-time metadata of the currently loaded data. */
  getRunTime(): string | null;
  /**
   * Sample the field at a lon/lat. Returns `null` outside the data
   * extent or before the field is ready. For wind this is wind speed +
   * "from" direction; for waves it's wave height + "from" direction.
   */
  sampleAt(lon: number, lat: number): {
    magnitude: number;
    fromDeg: number;
  } | null;
  /** Underlying ol-wind layer instance. */
  layer: any;
}

export interface SetupWeatherOptions {
  /** layerOptions name (e.g. "wind", "waves"). */
  layerName: string;
  /** UI label. */
  displayName: string;
  /** Optional parent layer name (e.g. "weather"). */
  parent?: string;
  /** Initial model identifier (must match a backend registry entry, e.g. "gfs"). */
  initialModel: string;
  /** Builds the backend URL for a given model + forecast hour. Returning
   *  null means the model is frontend-rendered (no JSON fetch) and the
   *  caller should swap renderers instead. */
  dataUrlFor: (modelName: string, fh: number) => string | null;
  /** Optional async fetcher that takes over from `dataUrlFor` when set.
   *  Used by tile-published models (ECMWF via R2 CDN) where the URL
   *  depends on runtime state (current viewport + latest cycle pointer)
   *  and may need a fallback to the legacy local endpoint on CDN error.
   *  Returning `{ data }` succeeds; returning `{ error }` surfaces the
   *  message in the UI same as a `dataUrlFor` HTTP failure would. */
  fetchData?: (modelName: string, fh: number) => Promise<{ data?: any; error?: string }>;
  /** ol-wind colour scale (15 colours typical, mapped linearly across [0, maxVelocity]). */
  colorScale: string[];
  /** Lower bound of the colour scale (m/s). */
  minVelocity: number;
  /** Upper bound of the colour scale (m/s). */
  maxVelocity: number;
  /** Multiplier applied to particle motion per frame. Zoom-aware for wind. */
  velocityScale: number | (() => number);
  /** Initial forecast hour (0 = analysis, 3 = +3 h, …). */
  initialForecastHour?: number;
  /** Particle path count. Lower for waves (denser data) than for wind. */
  paths?: number;
  /** Frames before a particle is re-randomized. */
  particleAge?: number;
  /** Particle line width in CSS pixels. */
  lineWidth?: number;
  /**
   * wind-core's `globalAlpha` knob. Doubles as the stroke alpha for new
   * particles AND the trail-fadeout rate, so higher = brighter strokes
   * but shorter trails. Default 0.82 reads as "atmospheric" wind;
   * waves need ~0.95 since the cyan/green low-end of the colour ramp
   * vanishes into the basemap at the lower alpha.
   */
  globalAlpha?: number;
  /** OL z-index. */
  zIndex?: number;
}

/** Fetch the JSON. Returns `{ data }` on success or `{ error }` with the
 *  upstream's error body so the caller can surface "model X is disabled
 *  because Y" in the UI rather than just blanking the layer. */
async function fetchJSON(url: string, fh: number): Promise<{ data?: any; error?: string }> {
  try {
    // Default `cache` (no override) — the server stamps Cache-Control:
    // max-age=300 on these responses, so the browser HTTP cache makes
    // return-scrubs to a recently-loaded hour effectively free instead
    // of re-downloading 35 MB of JSON.
    const resp = await fetch(`${url}?fh=${fh}`);
    if (!resp.ok) {
      const body = await resp.text().catch(() => "");
      return { error: body.trim() || `HTTP ${resp.status}` };
    }
    return { data: await resp.json() };
  } catch (e: any) {
    return { error: String(e?.message ?? e) };
  }
}

export async function setupWeatherLayer(
  mapGlobal: WindMapHandle,
  opts: SetupWeatherOptions,
): Promise<WeatherLayerHandle | null> {
  const mod: any = await import(/* @vite-ignore */ "ol-wind").catch(() => null);
  if (!mod) return null;
  const WindLayer = mod.WindLayer ?? mod.OlWind ?? mod.default;
  if (typeof WindLayer !== "function") {
    console.warn("ol-wind: unexpected module shape", Object.keys(mod));
    return null;
  }

  let currentFh = Math.max(0, opts.initialForecastHour ?? 0);
  let currentModel = opts.initialModel;
  const urlFor = (model: string, fh: number) => opts.dataUrlFor(model, fh);
  // Either fetch via the supplied async fetcher (tile-published models)
  // or via the synchronous URL builder + fetchJSON (everything else).
  // Both paths return the same { data?, error? } shape so the rest of
  // the loader doesn't need to care which one is wired up.
  const fetchOne = async (model: string, fh: number): Promise<{ data?: any; error?: string }> => {
    if (opts.fetchData) return opts.fetchData(model, fh);
    const url = urlFor(model, fh);
    if (!url) return { error: `model ${model} has no data endpoint (frontend-only)` };
    return fetchJSON(url, fh);
  };
  if (!opts.fetchData) {
    const initialUrl = urlFor(currentModel, currentFh);
    if (!initialUrl) {
      // The initial model has no fetch URL (frontend-rendered, e.g. WMS).
      // We can't set up a particle layer for that; bail.
      return null;
    }
  }

  // Per-handle LRU of parsed JSON keyed by `${model}:${fh}`. Wire payload
  // is ~35 MB raw / ~5 MB gzipped, and JSON.parse on the raw body is the
  // dominant cost on a return-scrub even with a warm browser cache —
  // hitting this map skips both the network roundtrip and the parse.
  // Map iteration order is insertion order, so delete-then-set on hit
  // implements LRU in O(1).
  const dataCache = new Map<string, any>();
  // 8 entries × 35 MB ≈ 280 MB for wind, ~80 MB for waves. Larger
  // caches don't usefully help — the user typically scrubs forward
  // through a forecast, then back, then forward, all within a small
  // window.
  const DATA_CACHE_MAX = 8;
  const cacheKey = (model: string, fh: number) => `${model}:${fh}`;
  function cacheGet(model: string, fh: number): any | null {
    const key = cacheKey(model, fh);
    const v = dataCache.get(key);
    if (v === undefined) return null;
    dataCache.delete(key);
    dataCache.set(key, v);
    return v;
  }
  function cachePut(model: string, fh: number, data: any) {
    const key = cacheKey(model, fh);
    if (dataCache.has(key)) dataCache.delete(key);
    dataCache.set(key, data);
    while (dataCache.size > DATA_CACHE_MAX) {
      const first = dataCache.keys().next().value;
      if (first === undefined) break;
      dataCache.delete(first);
    }
  }

  // Background prefetch of neighbouring hours. Slider step is 3h (the
  // GFS native step), so prefetching ±3 covers the most common scrub
  // pattern; ±6 buys a second hop for free. In-flight tracking dedupes
  // overlapping prefetches when the user keeps scrubbing.
  const inflightPrefetch = new Set<string>();
  function prefetch(model: string, fh: number) {
    if (fh < 0) return;
    const key = cacheKey(model, fh);
    if (dataCache.has(key)) return;
    if (inflightPrefetch.has(key)) return;
    if (!opts.fetchData) {
      const url = urlFor(model, fh);
      if (!url) return;
    }
    inflightPrefetch.add(key);
    fetchOne(model, fh)
      .then((r) => {
        if (r.data) cachePut(model, fh, r.data);
      })
      .catch(() => {})
      .finally(() => inflightPrefetch.delete(key));
  }
  function prefetchNeighbors(model: string, fh: number) {
    prefetch(model, fh + 3);
    prefetch(model, fh - 3);
    prefetch(model, fh + 6);
  }

  const first = await fetchOne(currentModel, currentFh);
  if (!first.data) return null;
  let data: any = first.data;
  cachePut(currentModel, currentFh, data);
  // Warm the next slider position so the first scrub is instant. Done
  // before windLayer construction since that step is synchronous and
  // doesn't block the in-flight prefetch.
  prefetchNeighbors(currentModel, currentFh);

  const windLayer = new WindLayer(data, {
    forceRender: true,
    fieldOptions: {
      translateX: true,
      wrapX: true,
      flipY: true,
    },
    windOptions: {
      velocityScale: opts.velocityScale,
      paths: opts.paths ?? 2500,
      particleAge: opts.particleAge ?? 100,
      lineWidth: opts.lineWidth ?? 1.6,
      // Lower globalAlpha lengthens the trails (more is kept frame-to-
      // frame) and softens each particle's per-frame stroke — together
      // that takes the animation from "frantic" to "atmospheric".
      globalAlpha: opts.globalAlpha ?? 0.82,
      colorScale: opts.colorScale,
      minVelocity: opts.minVelocity,
      maxVelocity: opts.maxVelocity,
    },
    zIndex: opts.zIndex ?? 30,
    opacity: 1,
  });
  let currentRunTime: string | null = data[0]?.header?.refTime ?? null;
  console.log(
    `${opts.layerName} layer ready fh=${currentFh} refTime=${currentRunTime}`,
  );

  // If the caller already pushed a placeholder entry for this layer
  // (so it sits next to its sibling in panel order), update it in
  // place. Otherwise push a fresh entry.
  const existing = mapGlobal.layerOptions.find(
    (l) => l.name === opts.layerName,
  );
  if (existing) {
    existing.layer = windLayer as unknown as BaseLayer;
    if (opts.parent !== undefined) existing.parent = opts.parent;
    if (existing.displayName === undefined)
      existing.displayName = opts.displayName;
  } else {
    mapGlobal.layerOptions.push({
      name: opts.layerName,
      displayName: opts.displayName,
      on: false,
      layer: windLayer as unknown as BaseLayer,
      parent: opts.parent,
    });
  }
  (window as any)[`__${opts.layerName}`] = windLayer;

  installProjectionPatches(windLayer);

  // Common path used by both setForecastHour and setModel — re-fetches
  // the JSON for the requested (model, fh) pair and re-binds the
  // ol-wind layer's data. Returns an error string on failure so the
  // UI can surface "this model is disabled because Y" instead of just
  // blanking the layer.
  async function loadInto(model: string, fh: number): Promise<string | null> {
    if (!opts.fetchData) {
      const url = urlFor(model, fh);
      if (!url) return `model ${model} has no data endpoint (frontend-only)`;
    }
    let parsed = cacheGet(model, fh);
    if (parsed === null) {
      const result = await fetchOne(model, fh);
      if (!result.data) return result.error ?? "fetch failed";
      parsed = result.data;
      cachePut(model, fh, parsed);
    }
    currentModel = model;
    currentFh = fh;
    currentRunTime = parsed[0]?.header?.refTime ?? null;
    windLayer.setData(parsed, {
      translateX: true,
      wrapX: true,
      flipY: true,
    });
    // setData rebinds the field but ol-wind doesn't re-patch its
    // projection methods — re-install ours and re-seed particles.
    installProjectionPatches(windLayer);
    console.log(
      `${opts.layerName} layer updated model=${model} fh=${fh} refTime=${currentRunTime}`,
    );
    prefetchNeighbors(model, fh);
    return null;
  }

  return {
    layer: windLayer,
    getForecastHour: () => currentFh,
    getModel: () => currentModel,
    getRunTime: () => currentRunTime,
    sampleAt(lon: number, lat: number) {
      const field = windLayer.getData?.();
      if (!field || typeof field.interpolatedValueAt !== "function") {
        return null;
      }
      const v = field.interpolatedValueAt(lon, lat);
      if (!v || typeof v.u !== "number" || typeof v.v !== "number") {
        return null;
      }
      const magnitude = Math.sqrt(v.u * v.u + v.v * v.v);
      // u = -mag·sin(dirFrom·π/180), v = -mag·cos(dirFrom·π/180), so
      // dirFrom = atan2(-u, -v) in radians, expressed mod 360.
      const fromDeg =
        (Math.atan2(-v.u, -v.v) * 180) / Math.PI;
      return {
        magnitude,
        fromDeg: (fromDeg + 360) % 360,
      };
    },
    async setForecastHour(hour: number) {
      // Snap to the model's published step (frontend has no way of
      // knowing the per-model StepFh, so we use a permissive 1 h step
      // here and let the backend resnap). Older callers passed GFS-
      // aligned 3 h values, which are already valid for every model.
      const fh = Math.max(0, Math.round(hour));
      if (fh === currentFh) return;
      await loadInto(currentModel, fh);
    },
    async setModel(modelName: string) {
      if (modelName === currentModel) return null;
      return await loadInto(modelName, currentFh);
    },
  };
}

/**
 * Replace ol-wind's project / unproject / intersectsCoordinate with
 * direct math that works under useGeographic. The originals call
 * `toUserCoordinate` + `transform("EPSG:4326")` which collapse to ~(0,0)
 * when the user projection is already EPSG:4326. Also fixes a separate
 * pixelRatio bug: wind-core's `randomize` passes canvas-backing pixels
 * to unproject (e.g. 1672 across a 2× display), but the original
 * unproject treats them as CSS pixels — so particles spawn off-canvas.
 */
function installProjectionPatches(windLayer: any): void {
  const apply = (): boolean => {
    const renderer: any = windLayer.getRenderer?.();
    if (!renderer || !renderer.wind) return false;
    const fsOf = () => renderer.frameState;
    renderer.wind.project = (coord: number[]): number[] | null => {
      const fs = fsOf();
      if (!fs) return null;
      const merc = projTransform(coord, "EPSG:4326", fs.viewState.projection);
      const pixel = applyTransform(fs.coordinateToPixelTransform, [
        merc[0],
        merc[1],
      ]);
      return [pixel[0] * fs.pixelRatio, pixel[1] * fs.pixelRatio];
    };
    renderer.wind.unproject = (pixel: number[]): number[] | null => {
      const fs = fsOf();
      if (!fs) return null;
      const cssPx = [pixel[0] / fs.pixelRatio, pixel[1] / fs.pixelRatio];
      const merc = applyTransform(fs.pixelToCoordinateTransform, cssPx);
      const lonlat = projTransform(merc, fs.viewState.projection, "EPSG:4326");
      return [lonlat[0], lonlat[1]];
    };
    renderer.wind.intersectsCoordinate = (coord: number[]): boolean => {
      const fs = fsOf();
      if (!fs) return true;
      const merc = projTransform(coord, "EPSG:4326", fs.viewState.projection);
      const ext = fs.extent;
      return (
        merc[0] >= ext[0] &&
        merc[0] <= ext[2] &&
        merc[1] >= ext[1] &&
        merc[1] <= ext[3]
      );
    };
    if (typeof renderer.wind.prerender === "function") {
      renderer.wind.prerender();
    }
    return true;
  };
  const tick = () => {
    if (!apply()) window.setTimeout(tick, 100);
  };
  tick();
}

// ---- Colour scales -------------------------------------------------------

/**
 * Wind colour scale: blue → green at 10 kt → yellow → orange → red.
 * Pinned to [0, 15 m/s] (= 0 to ~29 kt) for stable colours across panels.
 */
export const WIND_COLOR_SCALE = [
  "#0a3d91", // 0 kt — deep blue
  "#1565c0", // 2 kt
  "#1e88e5", // 4 kt
  "#4fc3f7", // 6 kt
  "#26a69a", // 8 kt — teal
  "#2e7d32", // 10 kt — solid green ★
  "#66bb6a", // 12 kt
  "#cddc39", // 14 kt — chartreuse
  "#fbc02d", // 16 kt — saturated yellow
  "#f57f17", // 18 kt
  "#e65100", // 20 kt — orange
  "#d84315", // 22 kt
  "#bf360c", // 24 kt
  "#b71c1c", // 26 kt — red
  "#7f0000", // 28+ kt — dark red
];

/**
 * Wave-height colour scale, pinned to [0, 3 m] (= 0..~10 ft) of the
 * "magnitude" slot we re-purposed for wave-height-in-metres.
 *
 * High-contrast against an ocean-blue basemap: calm seas read as
 * near-white rather than a blue that dissolves into the chart. Mid
 * range goes through saturated cyan → green → yellow → orange so even
 * thin particle streaks pop, and the top end saturates to deep red for
 * dangerous heights. 15 evenly-spaced stops so ol-wind's linear sample
 * lands roughly on the named heights below.
 */
export const WAVE_COLOR_SCALE = [
  "#f0f7ff", // 0.0 m (0 ft) — near-white (visible on blue basemap)
  "#c8e4ff", // 0.21
  "#8dcaff", // 0.43 — sky blue
  "#3eb1ff", // 0.64 (~2 ft, legend tick) — bright cyan
  "#1ad3c4", // 0.86 — saturated teal
  "#27d77a", // 1.07
  "#3ed24a", // 1.29 — bright green
  "#bde534", // 1.50 (~5 ft, legend tick) — chartreuse
  "#fff200", // 1.71 — saturated yellow
  "#ffb627", // 1.93
  "#ff7a1a", // 2.14 (~7 ft, legend tick) — saturated orange
  "#ff4d17", // 2.36
  "#e51d1d", // 2.57 — saturated red
  "#b51010", // 2.79
  "#6e0606", // 3.0+ m (~10 ft, legend tick) — deep red
];

/**
 * Resolve a numeric measurement to the colour ol-wind would render for
 * it. Used by the cursor-info readout to paint a small swatch next to
 * the wind / wave number so the eye can correlate the popup with the
 * gradient on screen.
 *
 * Linearly interpolates between adjacent stops so a 12 kt readout
 * doesn't snap to the colour of either 10 or 14 kt — same behaviour
 * ol-wind uses internally for the particle stroke.
 */
export function colorForValue(
  scale: string[],
  value: number,
  maxValue: number,
): string {
  if (scale.length === 0) return "#000";
  if (!Number.isFinite(value) || maxValue <= 0) return scale[0];
  const t = Math.max(0, Math.min(1, value / maxValue));
  const idx = t * (scale.length - 1);
  const i0 = Math.floor(idx);
  const i1 = Math.min(scale.length - 1, i0 + 1);
  const f = idx - i0;
  const a = parseHex(scale[i0]);
  const b = parseHex(scale[i1]);
  return `rgb(${Math.round(a[0] + (b[0] - a[0]) * f)},${Math.round(a[1] + (b[1] - a[1]) * f)},${Math.round(a[2] + (b[2] - a[2]) * f)})`;
}

function parseHex(h: string): [number, number, number] {
  const x = h.replace("#", "");
  return [
    parseInt(x.slice(0, 2), 16),
    parseInt(x.slice(2, 4), 16),
    parseInt(x.slice(4, 6), 16),
  ];
}
