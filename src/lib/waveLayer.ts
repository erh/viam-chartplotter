/**
 * Wave overlay using PacIOOS's NOAA WaveWatch III "best" THREDDS WMS.
 * Renders a semi-transparent significant-wave-height heatmap as
 * standard OpenLayers WMS tiles — no server-side proxy needed because
 * the PacIOOS endpoint is CORS-open. Cursor sampling goes through the
 * same WMS via GetFeatureInfo so the value the user reads matches the
 * colour they see.
 */
import type View from "ol/View";
import type BaseLayer from "ol/layer/Base";
import TileLayer from "ol/layer/Tile";
import TileWMS from "ol/source/TileWMS.js";
import { transform as projTransform } from "ol/proj.js";

/** Subset of marineMap's mapGlobal that we need. */
export interface WaveMapHandle {
  layerOptions: Array<{
    name: string;
    displayName?: string;
    on: boolean;
    layer?: any;
    parent?: string;
    minZoom?: number;
  }>;
}

export interface WaveLayerHandle {
  /** The TileWMS source — exposed for updateParams on TIME changes. */
  getSource(): TileWMS;
  /**
   * Update the wave WMS TIME parameter so the layer (and any
   * subsequent sampleAt) reflects the requested forecast time. The
   * server snaps to the nearest available slice in its dataset.
   */
  setTime(date: Date): void;
  /**
   * Sample the WMS at a lon/lat. Returns the wave height in metres
   * (or null on error / outside the model domain). Single-shot — the
   * caller is responsible for debouncing.
   */
  sampleAt(lonLat: [number, number], view: View): Promise<number | null>;
}

/** PacIOOS NOAA WaveWatch III "best" dataset WMS endpoint. */
export const WAVE_WMS_URL =
  "https://pae-paha.pacioos.hawaii.edu/thredds/wms/ww3_global/WaveWatch_III_Global_Wave_Model_best.ncd";

/**
 * Wave-height colour ramp range. Expressed in metres (PacIOOS units)
 * but the UI surfaces feet. Tighter than the previous 0..8 m so the
 * spectral palette lands its green ~2 ft / yellow ~5 ft like a
 * recreational sailor expects.
 */
export const WAVE_RANGE_MIN_M = 0;
export const WAVE_RANGE_MAX_M = 1.83; // ≈ 6 ft

/** Convenience constant for the cursor + legend conversions. */
export const METERS_TO_FEET = 3.28084;

/**
 * GetLegendGraphic URL for just the colour bar — no axis ticks or
 * units — so the host page can label it in feet alongside. Matches
 * the WMS PALETTE / NUMCOLORBANDS / COLORSCALERANGE used for tiles so
 * the legend strip lines up with the rendered map colours pixel-for-
 * pixel.
 */
export const WAVE_LEGEND_URL =
  `${WAVE_WMS_URL}?REQUEST=GetLegendGraphic&LAYER=Thgt` +
  `&PALETTE=rainbow&NUMCOLORBANDS=250` +
  `&COLORSCALERANGE=${WAVE_RANGE_MIN_M},${WAVE_RANGE_MAX_M}` +
  // Horizontal aspect (WIDTH > HEIGHT) so ncWMS hands back a
  // left-to-right gradient suitable for the chart's bottom strip.
  `&WIDTH=200&HEIGHT=16&COLORBARONLY=true` +
  // Bust any browser cache of the previous vertical-orientation URL
  // we shipped before flipping to horizontal.
  `&_v=h2`;

export function setupWaveLayer(mapGlobal: WaveMapHandle): WaveLayerHandle {
  // PacIOOS NOAA WaveWatch III "best" THREDDS WMS. ncWMS lets us crank
  // NUMCOLORBANDS up so the colour ramp doesn't band the way a
  // 20-step palette would, and the 128 px tileSize halves the covered
  // geographic area per request so each tile re-runs the server-side
  // resampling over a tighter window — at the cost of more HTTP
  // requests, the heatmap reads as finer rectangles instead of one
  // big mosaic block per tile.
  const source = new TileWMS({
    url: WAVE_WMS_URL,
    params: {
      LAYERS: "Thgt",
      // `rainbow` runs blue → cyan → green → yellow → red with no
      // purple/magenta at the bottom (`spectral` starts violet).
      STYLES: "boxfill/rainbow",
      FORMAT: "image/png",
      TRANSPARENT: true,
      VERSION: "1.3.0",
      COLORSCALERANGE: `${WAVE_RANGE_MIN_M},${WAVE_RANGE_MAX_M}`,
      NUMCOLORBANDS: 250,
      BELOWMINCOLOR: "transparent",
      ABOVEMAXCOLOR: "extend",
    },
    tileSize: [128, 128],
    crossOrigin: "anonymous",
    transition: 250,
  });
  const layer = new TileLayer({
    opacity: 0.55,
    preload: 1,
    zIndex: 28,
    source,
  });
  mapGlobal.layerOptions.push({
    name: "waves",
    displayName: "waves",
    parent: "weather",
    on: false,
    layer: layer as unknown as BaseLayer,
  });

  return {
    getSource: () => source,
    setTime(date: Date) {
      source.updateParams({ TIME: date.toISOString() });
    },
    async sampleAt(lonLat: [number, number], view: View) {
      const res = view.getResolution();
      const proj = view.getProjection();
      if (!res || !proj) return null;
      // OL expects coords in the view projection (mercator under
      // useGeographic, lat/lon at the API level). projTransform
      // converts the lon/lat the caller hands us into mercator.
      const merc = projTransform(lonLat, "EPSG:4326", proj);
      // ncWMS rejects application/json; only its built-in
      // FeatureInfoResponse XML format works. We parse the <value>
      // element out by hand below.
      const url = source.getFeatureInfoUrl(merc, res, proj, {
        INFO_FORMAT: "text/xml",
        FEATURE_COUNT: "1",
      });
      if (!url) return null;
      try {
        const resp = await fetch(url);
        if (!resp.ok) return null;
        const text = await resp.text();
        const doc = new DOMParser().parseFromString(text, "text/xml");
        const valueEl = doc.querySelector("FeatureInfo > value");
        const raw = valueEl?.textContent?.trim();
        const v = raw ? parseFloat(raw) : NaN;
        return Number.isFinite(v) ? v : null;
      } catch {
        return null;
      }
    },
  };
}
