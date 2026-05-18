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
  /** Refetch data at the given forecast hour (0..240, snapped to 3 h). */
  setForecastHour(hour: number): Promise<void>;
  /** Current forecast hour (in hours from the GFS run time). */
  getForecastHour(): number;
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
  /** Backend endpoint that returns the GFS-shape JSON. fh is appended as ?fh=N. */
  dataUrl: string;
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
  /** OL z-index. */
  zIndex?: number;
}

async function fetchJSON(url: string, fh: number): Promise<any | null> {
  try {
    const resp = await fetch(`${url}?fh=${fh}`, { cache: "no-store" });
    if (!resp.ok) return null;
    return await resp.json();
  } catch {
    return null;
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
  let data: any = await fetchJSON(opts.dataUrl, currentFh);
  if (!data) return null;

  const windLayer = new WindLayer(data, {
    forceRender: true,
    fieldOptions: {
      translateX: true,
      wrapX: true,
      flipY: true,
    },
    windOptions: {
      velocityScale: opts.velocityScale,
      paths: opts.paths ?? 4000,
      particleAge: opts.particleAge ?? 120,
      lineWidth: opts.lineWidth ?? 2.5,
      globalAlpha: 0.95,
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

  return {
    layer: windLayer,
    getForecastHour: () => currentFh,
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
      const fh = Math.max(0, Math.min(240, Math.round(hour / 3) * 3));
      if (fh === currentFh) return;
      const next = await fetchJSON(opts.dataUrl, fh);
      if (!next) return;
      currentFh = fh;
      currentRunTime = next[0]?.header?.refTime ?? null;
      windLayer.setData(next, {
        translateX: true,
        wrapX: true,
        flipY: true,
      });
      // setData rebinds the field but ol-wind doesn't re-patch its
      // projection methods — re-install ours and re-seed particles.
      installProjectionPatches(windLayer);
      console.log(
        `${opts.layerName} layer updated fh=${fh} refTime=${currentRunTime}`,
      );
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
 * Wave-height colour scale: calm cyan → light/yellow at 2 m → orange at
 * 4 m → red over 6 m. Pinned to [0, 8 m] of "magnitude" (the m/s slot
 * we re-purposed for wave-height-in-metres on the wave records).
 */
export const WAVE_COLOR_SCALE = [
  "#08306b", // 0.0 m
  "#08519c", // 0.5
  "#2171b5", // 1.0
  "#4292c6", // 1.5
  "#6baed6", // 2.0 m — light blue
  "#9ecae1", // 2.5
  "#c6dbef", // 3.0
  "#fdd49e", // 3.5 — pale orange
  "#fdae6b", // 4.0 m — orange
  "#fd8d3c", // 4.5
  "#f16913", // 5.0
  "#d94801", // 5.5
  "#a63603", // 6.0 m — red-brown
  "#7f2704", // 7.0
  "#4a0a00", // 8+ m — very dark red
];
