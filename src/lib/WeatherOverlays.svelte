<!--
  WeatherOverlays — all weather UI + logic for the chartplotter, extracted from
  marineMap.svelte. Owns the wind/wave particle layers, isobars, lightning, GOES
  satellite imagery, the GFS forecast-hour/run-time machinery, and the Open-Meteo
  current-conditions fetch. Registers its layers into the shared mapGlobal.
  layerOptions and drives the overlay UI (satellite controls, legends, forecast
  bar). Two values flow back to the parent via bind: cursorSampler (so the map's
  pointer handler can read wind/wave at a lon/lat without touching the handles)
  and weatherInfo (rendered in the parent's data panel).
-->
<script lang="ts">
  import type Map from "ol/Map";
  import type View from "ol/View";
  import type BaseLayer from "ol/layer/Base";
  import type { BoatInfo } from "./BoatInfo";
  import {
    setupWeatherLayer,
    WIND_COLOR_SCALE,
    WAVE_COLOR_SCALE,
    colorForValue,
    type WeatherLayerHandle,
  } from "./windLayer";
  import {
    setupIsobarLayer,
    type IsobarLayerHandle,
  } from "./isobarLayer";
  import TileLayer from "ol/layer/Tile";
  import XYZ from "ol/source/XYZ";

  // Minimal view of the parent's layerOptions entries the weather code touches.
  // (The parent's full LayerOption has more fields; structural typing lets us
  // accept it through this narrower shape.)
  interface LayerOption {
    name: string;
    displayName?: string;
    on?: boolean;
    layer?: BaseLayer;
    parent?: string;
    minZoom?: number;
    maxZoom?: number;
  }

  // Open-Meteo current-conditions snapshot, surfaced to the parent's data panel.
  export interface WeatherInfo {
    tempNowF: number | null;
    tempMinF: number;
    tempMaxF: number;
    windNowKn: number | null;
    windNowDirDeg: number | null;
    windMaxKn: number;
    rainTotalIn: number;
    rainHoursAny: number;
    sunriseLocal: string | null;
    sunsetLocal: string | null;
  }

  // Wind/wave sample at a lon/lat, published for the parent's cursor readout.
  export interface CursorWeatherSample {
    windKt: number | null;
    windFromDeg: number | null;
    waveM: number | null;
    waveFromDeg: number | null;
  }

  interface Props {
    // Shared reactive map state. We read .map/.view and read+write .layerOptions.
    mapGlobal: {
      map: Map | null;
      view: View | null;
      layerOptions: LayerOption[];
    };
    // Reactive view zoom (written by the parent). Gates the weather UI.
    currentZoom: number;
    // Boat prop, forwarded for the Open-Meteo refetch effect.
    myBoat?: BoatInfo;
    // tileBase-prefixing backend URL helper.
    api: (path: string) => string;
    // Whether the Go module's noaa-* endpoints are reachable on the current
    // origin. Gates weather layer setup so we don't fire /noaa-weather fetches
    // against an unreachable backend (matches the parent's setupLayers gate).
    noaaCacheReachable: () => boolean;
    // Bindable: child publishes a sampler the parent calls in its pointer handler.
    cursorSampler?: (lon: number, lat: number) => CursorWeatherSample;
    // Bindable: child publishes the Open-Meteo snapshot for the data panel.
    weatherInfo?: WeatherInfo | null;
  }

  let {
    mapGlobal,
    currentZoom,
    myBoat,
    api,
    noaaCacheReachable,
    cursorSampler = $bindable(),
    weatherInfo = $bindable(null),
  }: Props = $props();

  // Weather overlay state. Populated once GFS / GFSWAVE data + ol-wind
  // import resolve. Drives the shared forecast-hour slider beneath the
  // chart and the cursor wind/wave readout.
  let windHandle: WeatherLayerHandle | null = $state(null);
  let waveHandle: WeatherLayerHandle | null = $state(null);
  // Isobar layer (GFS PRMSL contours, GeoJSON LineStrings). Same
  // forecast-hour slider as the wind/wave layers, but no model picker —
  // there's only one PRMSL source registered today.
  let isobarHandle: IsobarLayerHandle | null = $state(null);
  let isobarLoading = $state(true);
  // Constants used by the wind/wave legend UIs. ol-wind's Field
  // carries the magnitude slot — wind speed in m/s for the wind layer,
  // wave height in m for the wave layer (we encoded h·sin/h·cos as u/v
  // on the backend). Both display ranges match the maxVelocity passed
  // to setupWeatherLayer further down so the legend gradient is
  // pixel-for-pixel what ol-wind paints.
  const WAVE_RANGE_MAX_M = 3;
  // 15 m/s ≈ 29.16 kt — the wind layer's maxVelocity. Rounded to 29
  // for the legend so the rightmost tick reads cleanly.
  const WIND_RANGE_MAX_KT = 29;
  const METERS_TO_FEET = 3.28084;
  const MS_TO_KT = 1.94384;
  // GFS forecast hour the slider is currently displaying. Snapped to a
  // 3 h step so changes line up with both wind + wave file cadences.
  let weatherForecastHour = $state(0);
  let weatherRunTime = $state<string | null>(null);
  // Per-layer fetch state. Default true because both setupWeatherLayer
  // calls fire at mount; flipped false in each .finally below. Reused
  // for refetches (model swap, forecast-hour scrub) so the spinner in
  // the forecast bar covers both initial load and subsequent fetches.
  let windLoading = $state(true);
  let waveLoading = $state(true);
  let weatherLoading = $derived(windLoading || waveLoading || isobarLoading);
  // First forecast hour ≥ "now", computed from the GFS run time. Used
  // as the slider minimum so the user can't scrub back into already-
  // expired analysis hours.
  let weatherMinForecastHour = $state(0);
  // Slider upper bound — driven by the currently-selected wind model's
  // MaxFh (ECMWF=144, GFS=240, HRRR=18, ICON-EU=120 …). Updated by the
  // model picker onchange handler so a GFS→ECMWF switch at fh=207
  // visibly snaps the slider to 144, instead of leaving the thumb at
  // a value the publisher has no tile for. Default 240 covers GFS at
  // startup before the per-model meta lands.
  let weatherMaxForecastHour = $state(240);
  // Highest zoom at which the GFS-resolution weather overlay reads as
  // signal rather than a flat coloured wash. Above this, both the
  // wind/wave layers and the forecast-hour slider hide automatically.
  const weatherMaxZoom = 12;
  // The set of wind / wave forecast models the user can switch between
  // — populated from /noaa-weather/models on mount. Each entry carries
  // its own MaxFh / StepFh / Disabled flag so the picker can both
  // display the human label and grey out models whose decoders haven't
  // been wired up server-side yet (NAM, ECMWF, ICON-Global).
  type WeatherModelMeta = {
    name: string;
    displayName: string;
    kind: "wind" | "wave" | "isobars" | "lightning";
    domain: string;
    minFh: number;
    maxFh: number;
    stepFh: number;
    disabled?: boolean;
    reason?: string;
  };
  let weatherModels = $state<WeatherModelMeta[]>([]);
  // Currently selected model per kind. Defaults to the legacy product
  // names so behaviour is unchanged until the user pops the dropdown.
  let windModel = $state("gfs");
  let waveModel = $state("pacioos-ww3");
  // Last error from a failed model switch, surfaced under the picker
  // so a user who picks a disabled stub can see *why* it's disabled
  // instead of just seeing the layer blank.
  let weatherModelError = $state<string | null>(null);
  // setWeatherError mirrors the user-facing banner string into the
  // browser console so the failure is grep-able + copy-pastable from
  // DevTools even when the banner times out / scrolls off.
  function setWeatherError(msg: string | null): void {
    weatherModelError = msg;
    if (msg) console.error("[weather]", msg);
  }

  // Convert a "GFS run time + forecast hour" pair into a Date in local
  // time, so we can show "Tue 14:00" instead of "+12h" on the slider.
  function weatherDataDate(runTimeIso: string | null, fh: number): Date | null {
    if (!runTimeIso) return null;
    const d = new Date(runTimeIso);
    if (Number.isNaN(d.getTime())) return null;
    return new Date(d.getTime() + fh * 3600_000);
  }

  // Switch the wave layer to a different upstream model. Today there's
  // only one registered wave option (pacioos-ww3) so the dropdown
  // doesn't render — this stays as a one-liner forwarder so adding a
  // second wave model (ECMWF WAM, GFSWAVE once we have JPEG2000) just
  // works without UI changes.
  async function swapWaveModel(next: string): Promise<string | null> {
    if (next === waveModel || !waveHandle) return null;
    const err = await waveHandle.setModel(next);
    if (!err) waveModel = next;
    return err;
  }

  // Compute the smallest 3-h-aligned forecast hour that lands at or
  // after "now" — used as the slider floor so the user always starts
  // from "current" rather than from the GFS analysis (which may be
  // several hours in the past).
  function nowForecastHour(runTimeIso: string | null): number {
    if (!runTimeIso) return 0;
    const run = new Date(runTimeIso);
    if (Number.isNaN(run.getTime())) return 0;
    const hoursFromRun = (Date.now() - run.getTime()) / 3600_000;
    return Math.max(0, Math.ceil(hoursFromRun / 3) * 3);
  }

  // Forecast hours that land on local midnight inside [minFh, maxFh], with
  // a weekday label for each. Used to overlay day-boundary ticks on the
  // weather slider so the user can see at a glance where Tuesday ends and
  // Wednesday begins, regardless of timezone or run-time alignment.
  function computeDayMarkers(
    runTimeIso: string | null,
    minFh: number,
    maxFh: number,
  ): Array<{ pct: number; label: string }> {
    if (!runTimeIso || maxFh <= minFh) return [];
    const run = new Date(runTimeIso);
    if (Number.isNaN(run.getTime())) return [];
    const sliderEnd = new Date(run.getTime() + maxFh * 3600_000).getTime();
    // First local midnight strictly after the slider's start; setHours(24,...)
    // lands on the next calendar day's 00:00 local. From there step by 24 h
    // (real-clock) — accepts the ~1 px slop on DST-transition days.
    const sliderStart = new Date(run.getTime() + minFh * 3600_000);
    const first = new Date(sliderStart);
    first.setHours(24, 0, 0, 0);
    const out: Array<{ pct: number; label: string }> = [];
    for (
      let t = first.getTime();
      t <= sliderEnd;
      t += 24 * 3600_000
    ) {
      const fh = (t - run.getTime()) / 3600_000;
      const pct = ((fh - minFh) / (maxFh - minFh)) * 100;
      const label = new Date(t).toLocaleDateString(undefined, {
        weekday: "short",
      });
      out.push({ pct, label });
    }
    return out;
  }

  // off → on. The forecast-hour slider only syncs visible layers
  // (each layer's setData blocks the main thread, so syncing
  // hidden ones would stack onto the user's wait), which means a
  // hidden layer can lag the slider value. We track effective-on
  // (parent && self) across runs and trigger a one-shot fetch on
  // the false → true edge. Idempotent: setForecastHour returns
  // immediately when fh already matches.
  let prevWindOn = $state(false);
  let prevWaveOn = $state(false);
  let prevIsobarOn = $state(false);
  $effect(() => {
    const parent = mapGlobal.layerOptions.find((l) => l.name === "weather");
    const wind = mapGlobal.layerOptions.find((l) => l.name === "wind");
    const wave = mapGlobal.layerOptions.find((l) => l.name === "waves");
    const isobar = mapGlobal.layerOptions.find((l) => l.name === "isobars");
    const parentOn = parent?.on ?? true;
    const windOn = parentOn && !!wind?.on;
    const waveOn = parentOn && !!wave?.on;
    const isobarOn = parentOn && !!isobar?.on;
    if (
      windOn && !prevWindOn && windHandle &&
      windHandle.getForecastHour() !== weatherForecastHour
    ) {
      windLoading = true;
      windHandle
        .setForecastHour(weatherForecastHour)
        .catch(() => {})
        .finally(() => { windLoading = false; });
    }
    if (
      waveOn && !prevWaveOn && waveHandle &&
      waveHandle.getForecastHour() !== weatherForecastHour
    ) {
      waveLoading = true;
      waveHandle
        .setForecastHour(weatherForecastHour)
        .catch(() => {})
        .finally(() => { waveLoading = false; });
    }
    if (
      isobarOn && !prevIsobarOn && isobarHandle &&
      isobarHandle.getForecastHour() !== weatherForecastHour
    ) {
      isobarLoading = true;
      isobarHandle
        .setForecastHour(weatherForecastHour)
        .catch(() => {})
        .finally(() => { isobarLoading = false; });
    }
    prevWindOn = windOn;
    prevWaveOn = waveOn;
    prevIsobarOn = isobarOn;
  });

  // GOES-East ABI imagery from NASA GIBS — free, no API key. Three bands
  // (GeoColor / Visible / Clean IR). Each band's WMTS tile matrix set caps
  // at a native max zoom (Level6/Level7) and OpenLayers overzooms it above
  // that — so each band sets its own source maxZoom. All PNG, ~10-min
  // cadence. GIBS has gaps and TIME=default is the latest valid frame; we
  // probe backward from now to find valid frames, then drive the layer's
  // TIME from a slider.
  const SAT_GIBS_BASE = "https://gibs.earthdata.nasa.gov/wmts/epsg3857/best";
  type SatBand = "blue" | "vis" | "infra";
  const SAT_BANDS: Record<
    SatBand,
    { layer: string; tms: string; maxZoom: number; label: string }
  > = {
    blue: {
      layer: "GOES-East_ABI_GeoColor",
      tms: "GoogleMapsCompatible_Level7",
      maxZoom: 7,
      label: "Color",
    },
    vis: {
      layer: "GOES-East_ABI_Band2_Red_Visible_1km",
      tms: "GoogleMapsCompatible_Level7",
      maxZoom: 7,
      label: "Visible",
    },
    infra: {
      layer: "GOES-East_ABI_Band13_Clean_Infrared",
      tms: "GoogleMapsCompatible_Level6",
      maxZoom: 6,
      label: "Infrared",
    },
  };
  let satBand = $state<SatBand>("blue");
  const satTileUrl = (time: string, band: SatBand = satBand) => {
    const b = SAT_BANDS[band];
    return `${SAT_GIBS_BASE}/${b.layer}/default/${time}/${b.tms}/{z}/{y}/{x}.png`;
  };
  const satProbeUrl = (time: string, band: SatBand = satBand) => {
    const b = SAT_BANDS[band];
    return `${SAT_GIBS_BASE}/${b.layer}/default/${time}/${b.tms}/0/0/0.png`;
  };
  const satIso = (d: Date) => d.toISOString().replace(/\.\d+Z$/, "Z");

  let satFrames = $state<string[]>([]); // valid ISO times, oldest → newest
  let satFrameIdx = $state(0); // index into satFrames; defaults to newest
  let satProbed = false;

  function fmtSatTime(t: string | undefined): string {
    if (!t) return "";
    return new Date(t).toLocaleTimeString([], {
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  // Probe 10-min steps back from now; keep the valid frames until we've spanned
  // ~2 h from the newest valid one. Runs once, when the satellite layer is first
  // enabled (see the $effect below).
  async function discoverSatelliteFrames() {
    const stepMs = 10 * 60 * 1000;
    const base = Math.floor(Date.now() / stepMs) * stepMs;
    const valid: string[] = [];
    let newestTs = 0;
    for (let k = 0; k <= 24; k++) {
      const t = satIso(new Date(base - k * stepMs));
      try {
        const r = await fetch(satProbeUrl(t), { cache: "no-store" });
        if (r.ok) {
          if (!newestTs) newestTs = new Date(t).getTime();
          valid.push(t);
          if (newestTs - new Date(t).getTime() >= 2 * 3600 * 1000) break;
        }
      } catch {
        // network hiccup — skip this candidate
      }
    }
    if (valid.length > 0) {
      satFrames = valid.slice().reverse(); // oldest → newest
      satFrameIdx = satFrames.length - 1;
      applySatelliteFrame();
    }
  }

  function applySatelliteFrame() {
    const entry = mapGlobal.layerOptions.find((l) => l.name === "satellite");
    const src = (entry?.layer as TileLayer<XYZ> | undefined)?.getSource?.();
    const t = satFrames[satFrameIdx];
    if (src && t) {
      src.setUrl(satTileUrl(t));
      src.refresh();
    }
  }

  // Switch the satellite band. The bands differ in native max zoom (IR is
  // coarser), which is fixed at source-construction time, so we swap in a fresh
  // XYZ source rather than just changing the URL. Reuses the discovered frame
  // times — GOES publishes every band on the same ~10-min cadence.
  function setSatelliteBand(band: SatBand) {
    if (band === satBand) return;
    satBand = band;
    const entry = mapGlobal.layerOptions.find((l) => l.name === "satellite");
    const layer = entry?.layer as TileLayer<XYZ> | undefined;
    if (!layer) return;
    const t = satFrames[satFrameIdx] ?? "default";
    layer.setSource(
      new XYZ({
        url: satTileUrl(t, band),
        crossOrigin: "anonymous",
        transition: 300,
        maxZoom: SAT_BANDS[band].maxZoom,
      }),
    );
  }

  // Lazily discover frames the first time the satellite layer is turned on, so
  // we don't probe GIBS on every load.
  $effect(() => {
    const on = mapGlobal.layerOptions.find((l) => l.name === "satellite")?.on;
    if (on && !satProbed) {
      satProbed = true;
      discoverSatelliteFrames();
    }
  });

  // Pushes the weather layerOptions placeholders + fires the async layer setup.
  // Gated on mapGlobal.map existing (so tileBase is resolved and api() points at
  // the right origin) and on the NOAA cache being reachable (matches the parent's
  // setupLayers gate). Each push is guarded by a .find so a re-run can't duplicate.
  function setupWeatherLayers() {
    // Weather overlays (wind + waves), both backed by NOMADS GFS /
    // GFSWAVE data via the bundled cache and rendered by ol-wind. The
    // "weather" parent is a folder toggle so the wind / waves
    // children can be enabled independently. All three default off.
    // "weather" is a folder-style parent (no actual layer of its own)
    // grouping the wind + waves children in the layer panel. Default
    // ON so the children are immediately togglable — turning weather
    // off disables both children at once, the standard parent/child
    // behaviour the panel already implements.
    if (!mapGlobal.layerOptions.find((l) => l.name === "weather")) {
      mapGlobal.layerOptions.push({
        name: "weather",
        displayName: "weather",
        on: true,
      });
    }
    // Pre-allocate the wind + waves entries so their panel rows sit
    // directly under the weather header — setupWeatherLayer is async
    // and would otherwise push them last (after boat / ais). The
    // actual OL layer reference is filled in by setupWeatherLayer
    // when each respective fetch returns.
    if (!mapGlobal.layerOptions.find((l) => l.name === "wind")) {
      mapGlobal.layerOptions.push({
        name: "wind",
        displayName: "wind",
        parent: "weather",
        on: false,
        maxZoom: weatherMaxZoom,
      });
    }
    if (!mapGlobal.layerOptions.find((l) => l.name === "waves")) {
      mapGlobal.layerOptions.push({
        name: "waves",
        displayName: "waves",
        parent: "weather",
        on: false,
        maxZoom: weatherMaxZoom,
      });
    }
    // Isobar overlay (GFS PRMSL contours). Placeholder row so the
    // panel order matches wind/waves; setupIsobarLayer fills in the
    // .layer reference once the first GeoJSON fetch resolves.
    if (!mapGlobal.layerOptions.find((l) => l.name === "isobars")) {
      mapGlobal.layerOptions.push({
        name: "isobars",
        displayName: "isobars",
        parent: "weather",
        on: false,
        maxZoom: weatherMaxZoom,
      });
    }
    // Lightning overlay. Backed by the noaa-glm stub model server-
    // side — the option appears in the panel but turning it on
    // surfaces the "needs NetCDF decoder" reason. Filled in for real
    // when the GLM decoder ships.
    if (!mapGlobal.layerOptions.find((l) => l.name === "lightning")) {
      mapGlobal.layerOptions.push({
        name: "lightning",
        displayName: "lightning",
        parent: "weather",
        on: false,
        maxZoom: weatherMaxZoom,
      });
    }
    // Satellite (infrared cloud) imagery overlay. Placeholder row so it sits
    // under the weather header; setupSatelliteLayer fills the .layer ref once
    // the RainViewer frame list resolves.
    if (!mapGlobal.layerOptions.find((l) => l.name === "satellite")) {
      mapGlobal.layerOptions.push({
        name: "satellite",
        displayName: "satellite",
        parent: "weather",
        on: false,
        maxZoom: weatherMaxZoom,
      });
    }
    const ensureRendered = () => {
      mapGlobal.map?.render();
    };
    const initialFh = nowForecastHour(null); // 0 until we know the run time
    // All wind models (incl. ECMWF) are served from Mongo by weathersync
    // via /noaa-weather/data/{model}/latest.json. No CDN/tile path.
    setupWindLayer();
    setupSatelliteLayer();

    // GOES-East GeoColor (visible/IR cloud composite) satellite imagery from
    // NASA GIBS — free, no API key. TIME=default serves the latest near-real-
    // time frame. Native to z7 (GoogleMapsCompatible_Level7); OL overzooms
    // above that. Covers the Americas/Atlantic (the GOES-East footprint),
    // ideal for US coastal waters. Semi-transparent so the chart shows through.
    function setupSatelliteLayer() {
      const layer = new TileLayer({
        opacity: 0.6,
        zIndex: 6,
        source: new XYZ({
          url: satTileUrl("default"),
          crossOrigin: "anonymous",
          transition: 300,
          maxZoom: SAT_BANDS[satBand].maxZoom,
        }),
      });
      const existing = mapGlobal.layerOptions.find(
        (l) => l.name === "satellite",
      );
      if (existing) existing.layer = layer;
    }

    function setupWindLayer() {
      setupWeatherLayer(mapGlobal, {
      layerName: "wind",
      displayName: "wind",
      parent: "weather",
      initialModel: windModel,
      dataUrlFor: (model, fh) => api(`/noaa-weather/data/${model}/latest.json`),
      fetchData: async (model, fh) => {
        const resp = await fetch(api(`/noaa-weather/data/${model}/latest.json?fh=${fh}`));
        if (!resp.ok) {
          const body = await resp.text().catch(() => "");
          return { error: body.trim() || `HTTP ${resp.status}` };
        }
        return { data: await resp.json() };
      },
      colorScale: WIND_COLOR_SCALE,
      minVelocity: 0,
      maxVelocity: 15,
      // Particle motion is in degrees under useGeographic. Halved
      // from ol-wind's 0.3 default to 0.225 / 2^z so a 10 m/s wind
      // drifts ~1.5 px / frame — between "creeping" (0.15) and
      // "darting" (0.3). High-wind streaks still come out
      // noticeably longer than calm-air streaks because the per-
      // frame distance is `velocityScale * magnitude`.
      velocityScale: () => {
        const z = mapGlobal.view?.getZoom() ?? 6;
        return 0.225 / Math.pow(2, z);
      },
      // Per-particle stroke width keyed off wind magnitude (m/s,
      // ≤ 15 per maxVelocity). ~2.7 px at calm air, ~4.35 px at
      // gale strength — midway between the prior constant 2.4 px
      // line and the more aggressive 3..6.3 px ramp, so faster
      // wind reads as thicker streaks without overwhelming the
      // chart underneath.
      lineWidth: (m: number) => 2.7 + Math.max(0, m) * 0.11,
      initialForecastHour: initialFh,
    })
      .then((wind) => {
        windHandle = wind;
        const refTime = wind?.getRunTime() ?? null;
        weatherRunTime = refTime;
        const floor = nowForecastHour(refTime);
        weatherMinForecastHour = floor;
        // Slider max tracks the active wind model's MaxFh from
        // /noaa-weather/models meta. Falls back to 240 (GFS) if
        // the models endpoint hasn't responded yet at this point.
        const initialMeta = weatherModels.find((m) => m.name === windModel);
        if (initialMeta) weatherMaxForecastHour = initialMeta.maxFh;
        weatherForecastHour = floor;
        // Re-fetch at the "now-aligned" hour if the initial f000
        // fetch happened before we knew the run time.
        if (floor > 0 && wind) wind.setForecastHour(floor).catch(() => {});
        // Bump the wave layer to the same forecast hour the wind
        // slider settled on, so both display the same future time.
        if (waveHandle && floor > 0) {
          waveHandle.setForecastHour(floor).catch(() => {});
        }
        ensureRendered();
      })
      .catch((err) => {
        console.warn("wind layer disabled:", err);
      })
      .finally(() => {
        windLoading = false;
      });
    } // end setupWindLayer
    // Wave overlay: ol-wind particle animation driven by Thgt+Tdir
    // from PacIOOS WaveWatch III, fetched server-side via OPeNDAP
    // and converted to u/v vectors so it shares the same JSON shape
    // and rendering pipeline as the wind layer. Slower velocityScale
    // since wave-propagation speed is a fraction of wind speed, and
    // a different colour ramp keyed to typical wave heights (0..3 m).
    setupWeatherLayer(mapGlobal, {
      layerName: "waves",
      displayName: "waves",
      parent: "weather",
      initialModel: waveModel,
      dataUrlFor: (model) => api(`/noaa-weather/data/${model}/latest.json`),
      colorScale: WAVE_COLOR_SCALE,
      minVelocity: 0,
      maxVelocity: 3,
      velocityScale: () => {
        const z = mapGlobal.view?.getZoom() ?? 6;
        // Was 0.06 / 2^z; bumped so a 1.5 m wave-height "particle"
        // drifts visibly each frame — wave-celerity isn't literally
        // proportional to height, but the eye reads the slow streaks
        // as "no data" otherwise.
        return 0.12 / Math.pow(2, z);
      },
      paths: 6000,
      lineWidth: 7.5,
      // Brighter strokes than wind — the calm/cyan end of the wave
      // ramp washes out against the ocean basemap otherwise.
      globalAlpha: 0.97,
      initialForecastHour: initialFh,
      zIndex: 29,
    })
      .then((wave) => {
        waveHandle = wave;
        if (wave && weatherForecastHour > 0) {
          wave.setForecastHour(weatherForecastHour).catch(() => {});
        }
      })
      .catch((err) => {
        console.warn("wave layer disabled:", err);
      })
      .finally(() => {
        waveLoading = false;
      });
    // Isobar overlay (GFS PRMSL contours). Same backend cache as
    // wind/waves, but the model returns GeoJSON LineStrings instead
    // of ol-wind records — handled by setupIsobarLayer with a thin
    // OL Vector layer. Forecast-hour scrubs are driven by the same
    // slider via the lockstep handler further down.
    setupIsobarLayer(mapGlobal, {
      layerName: "isobars",
      displayName: "isobars",
      parent: "weather",
      model: "gfs-isobars",
      initialForecastHour: initialFh,
      maxZoom: weatherMaxZoom,
    })
      .then((handle) => {
        isobarHandle = handle;
        if (handle && weatherForecastHour > 0) {
          handle.setForecastHour(weatherForecastHour).catch(() => {});
        }
      })
      .catch((err) => {
        console.warn("isobar layer disabled:", err);
      })
      .finally(() => {
        isobarLoading = false;
      });
    // Populate the model picker. Failures are non-fatal — the picker
    // just stays at the bundled defaults (GFS + PacIOOS) if the
    // registry endpoint isn't reachable for some reason.
    fetch(api("/noaa-weather/models"))
      .then((r) => (r.ok ? r.json() : []))
      .then((list: WeatherModelMeta[]) => {
        weatherModels = Array.isArray(list) ? list : [];
      })
      .catch(() => {});
  }

  // Drive setup once the map exists (so api() resolves the right origin) and
  // the NOAA cache is reachable. The map is built last in the parent's async
  // setupMap, after loadAppConfig + probeNoaaCache resolve.
  let weatherSetupDone = false;
  $effect(() => {
    if (mapGlobal.map && !weatherSetupDone && noaaCacheReachable()) {
      weatherSetupDone = true;
      setupWeatherLayers();
    }
  });

  // ---- Weather (Open-Meteo, no API key needed). Fetched directly from
  // the browser; refetched when the boat moves ~6 nm or every 30 minutes.
  let lastWeatherFetchKey = "";
  let weatherRefetchTimer: ReturnType<typeof setInterval> | null = null;

  async function refreshWeather(lat: number, lng: number): Promise<void> {
    try {
      const url =
        `https://api.open-meteo.com/v1/forecast` +
        `?latitude=${lat.toFixed(4)}&longitude=${lng.toFixed(4)}` +
        `&current=temperature_2m,wind_speed_10m,wind_direction_10m` +
        `&hourly=temperature_2m,precipitation,wind_speed_10m` +
        `&daily=sunrise,sunset` +
        `&temperature_unit=fahrenheit&wind_speed_unit=kn&precipitation_unit=inch` +
        `&timezone=auto&forecast_hours=4&forecast_days=2`;
      const r = await fetch(url);
      if (!r.ok) throw new Error(`weather http ${r.status}`);
      const j = await r.json();

      const cur = j.current ?? {};
      const hourly = j.hourly ?? {};
      const daily = j.daily ?? {};

      const temps: number[] = (hourly.temperature_2m ?? [])
        .filter((v: any) => typeof v === "number");
      const winds: number[] = (hourly.wind_speed_10m ?? [])
        .filter((v: any) => typeof v === "number");
      const rains: number[] = (hourly.precipitation ?? [])
        .filter((v: any) => typeof v === "number");

      // Pick today's sunrise/sunset relative to "now". Open-Meteo returns an
      // array indexed by day; the [0] entry is today in the requested tz, but
      // if today's sunset has already passed we'd rather show tomorrow's so
      // the panel stays useful all night.
      const nowMs = Date.now();
      const sunriseArr: string[] = daily.sunrise ?? [];
      const sunsetArr: string[] = daily.sunset ?? [];
      let dayIdx = 0;
      if (sunsetArr[0] && new Date(sunsetArr[0]).getTime() < nowMs) dayIdx = 1;

      const fmtLocalTime = (iso: string | undefined): string | null => {
        if (!iso) return null;
        // Open-Meteo's ISO strings come without a tz suffix, so JS treats
        // them as local — which is what we want since timezone=auto already
        // returned them in the boat's local time.
        const d = new Date(iso);
        if (Number.isNaN(d.getTime())) return null;
        return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
      };

      weatherInfo = {
        tempNowF: typeof cur.temperature_2m === "number" ? cur.temperature_2m : null,
        tempMinF: temps.length ? Math.min(...temps) : 0,
        tempMaxF: temps.length ? Math.max(...temps) : 0,
        windNowKn: typeof cur.wind_speed_10m === "number" ? cur.wind_speed_10m : null,
        windNowDirDeg:
          typeof cur.wind_direction_10m === "number" ? cur.wind_direction_10m : null,
        windMaxKn: winds.length ? Math.max(...winds) : 0,
        rainTotalIn: rains.reduce((a, b) => a + b, 0),
        rainHoursAny: rains.filter((v) => v > 0).length,
        sunriseLocal: fmtLocalTime(sunriseArr[dayIdx]),
        sunsetLocal: fmtLocalTime(sunsetArr[dayIdx]),
      };
    } catch (e) {
      console.warn("weather fetch failed", e);
    }
  }

  // Same trigger pattern as tide: refetch on ~6 nm location change, plus a
  // 30-minute background timer so the forecast stays fresh at anchor.
  $effect(() => {
    if (!myBoat?.location) return;
    const [lat, lng] = myBoat.location;
    if (lat === 0 && lng === 0) return;
    const key = `${Math.round(lat * 10) / 10},${Math.round(lng * 10) / 10}`;
    if (key === lastWeatherFetchKey) return;
    lastWeatherFetchKey = key;
    refreshWeather(lat, lng);
    if (weatherRefetchTimer) clearInterval(weatherRefetchTimer);
    weatherRefetchTimer = setInterval(
      () => {
        const loc = myBoat?.location;
        if (loc && !(loc[0] === 0 && loc[1] === 0)) {
          refreshWeather(loc[0], loc[1]);
        }
      },
      30 * 60 * 1000
    );
    return () => {
      if (weatherRefetchTimer) {
        clearInterval(weatherRefetchTimer);
        weatherRefetchTimer = null;
      }
    };
  });

  // Publish a wind/wave sampler for the parent's pointer handler. Reads the
  // handles (which stay private to this component) and returns the same shape
  // the parent's cursorInfo expects. The parent reads this prop at call time.
  $effect(() => {
    cursorSampler = (lon: number, lat: number): CursorWeatherSample => {
      let windKt: number | null = null;
      let windFromDeg: number | null = null;
      let waveM: number | null = null;
      let waveFromDeg: number | null = null;
      const layerOn = (n: string) =>
        !!mapGlobal.layerOptions.find((l) => l.name === n)?.on;
      if (layerOn("wind") && windHandle) {
        const s = windHandle.sampleAt(lon, lat);
        if (s) {
          windKt = s.magnitude * 1.94384;
          windFromDeg = s.fromDeg;
        }
      }
      if (layerOn("waves") && waveHandle) {
        const s = waveHandle.sampleAt(lon, lat);
        if (s) {
          // Backend encodes wave HEIGHT as the magnitude slot, so
          // s.magnitude is in metres. fromDeg is the direction-from
          // we want to surface.
          waveM = s.magnitude;
          waveFromDeg = s.fromDeg;
        }
      }
      return { windKt, windFromDeg, waveM, waveFromDeg };
    };
  });
</script>

<!-- Satellite controls: band selector (Color / Visible / Infrared) shown
     whenever the satellite layer is on, plus a time scrubber once we've
     discovered more than one frame (slide left to step back up to ~2 h). -->
{#if mapGlobal.layerOptions.find((l) => l.name === "satellite")?.on}
  <div class="sat-controls">
    <div class="sat-band" role="group" aria-label="Satellite band">
      {#each Object.entries(SAT_BANDS) as [band, cfg]}
        <button
          type="button"
          class="sat-band-btn"
          class:active={satBand === band}
          onclick={() => setSatelliteBand(band as SatBand)}
        >
          {cfg.label}
        </button>
      {/each}
    </div>
    {#if satFrames.length > 1}
      <div class="sat-time">
        <input
          class="sat-time-range"
          type="range"
          min="0"
          max={satFrames.length - 1}
          value={satFrameIdx}
          oninput={(e) => {
            satFrameIdx = +e.currentTarget.value;
            applySatelliteFrame();
          }}
          aria-label="Satellite time"
        />
        <span class="sat-time-label">
          {fmtSatTime(satFrames[satFrameIdx])}{satFrameIdx ===
          satFrames.length - 1
            ? " (latest)"
            : ""}
        </span>
      </div>
    {/if}
  </div>
{/if}

{#if (mapGlobal.layerOptions.find((l) => l.name === "wind")?.on || mapGlobal.layerOptions.find((l) => l.name === "waves")?.on) && currentZoom <= weatherMaxZoom}
  <!-- Stacked weather legends. Wind on top, waves below — both are
       pure-CSS horizontal gradients matched to the colour scales
       exported from windLayer.ts so a glance at the legend matches
       what the particle layer paints. Avoiding the WMS
       GetLegendGraphic PNG sidesteps a caching bug where the legacy
       14×140 vertical-orientation response kept rendering even
       after the URL changed to 200×16 horizontal. Sits above OL's
       ScaleLine (bottom-left). -->
  <div class="weather-legend-stack">
    {#if mapGlobal.layerOptions.find((l) => l.name === "wind")?.on}
      <div class="weather-legend">
        <div class="weather-legend-strip weather-legend-strip-wind"></div>
        <div class="weather-legend-ticks">
          {#each [0, WIND_RANGE_MAX_KT * 0.25, WIND_RANGE_MAX_KT * 0.5, WIND_RANGE_MAX_KT * 0.75, WIND_RANGE_MAX_KT] as kt}
            <span>{Math.round(kt)} kt</span>
          {/each}
        </div>
      </div>
    {/if}
    {#if mapGlobal.layerOptions.find((l) => l.name === "waves")?.on}
      <div class="weather-legend">
        <div class="weather-legend-strip weather-legend-strip-wave"></div>
        <div class="weather-legend-ticks">
          {#each [0, WAVE_RANGE_MAX_M * 0.25, WAVE_RANGE_MAX_M * 0.5, WAVE_RANGE_MAX_M * 0.75, WAVE_RANGE_MAX_M] as m}
            <span>{Math.round(m * METERS_TO_FEET)} ft</span>
          {/each}
        </div>
      </div>
    {/if}
  </div>
{/if}

{#if (windHandle || waveHandle) && (mapGlobal.layerOptions.find((l) => l.name === "wind")?.on || mapGlobal.layerOptions.find((l) => l.name === "waves")?.on) && currentZoom <= weatherMaxZoom}
  {@const previewDate = weatherDataDate(weatherRunTime, weatherForecastHour)}
  {@const windModelOptions = weatherModels.filter((m) => m.kind === "wind")}
  {@const waveModelOptions = weatherModels.filter((m) => m.kind === "wave")}
  {@const dayMarkers = computeDayMarkers(
    weatherRunTime,
    weatherMinForecastHour,
    weatherMaxForecastHour,
  )}
  <div class="wind-forecast-bar">
    {#if mapGlobal.layerOptions.find((l) => l.name === "wind")?.on && windModelOptions.length > 1}
      <!-- Wind model picker. Disabled-stub models stay listed (so the
           user sees what's *planned*) but the option is greyed and a
           selection attempt surfaces the registered Reason. -->
      <label class="wind-forecast-bar-model">
        <span class="wind-forecast-bar-model-prefix">wind</span>
        <select
          disabled={weatherLoading}
          value={windModel}
          onchange={async (e) => {
            const next = (e.currentTarget as HTMLSelectElement).value;
            windLoading = true;
            setWeatherError(null);
            try {
              const err = await windHandle?.setModel(next);
              if (err) {
                setWeatherError(`${next}: ${err}`);
                // Snap the select back to whatever the handle is
                // actually serving so the UI doesn't lie about the
                // current state.
                windModel = windHandle?.getModel() ?? windModel;
              } else {
                windModel = next;
                weatherRunTime = windHandle?.getRunTime() ?? weatherRunTime;
                // Re-floor the slider — different models have
                // different run cadences, so the "now hour" shifts.
                const floor = nowForecastHour(weatherRunTime);
                weatherMinForecastHour = floor;
                // Update the slider's MAX too — ECMWF caps at 144,
                // GFS at 240, HRRR at 18. Drives the visible track
                // length AND the value the thumb can be dragged to,
                // so the user can't even select an fh the publisher
                // doesn't have.
                const newModelMeta = weatherModels.find((m) => m.name === next);
                const newMaxFh = newModelMeta?.maxFh ?? 240;
                weatherMaxForecastHour = newMaxFh;
                // Clamp the current value to the new [floor, newMaxFh]
                // window. Without the down-clamp, switching GFS@fh=207
                // → ECMWF would silently 404 on every tile.
                let target = weatherForecastHour;
                if (target < floor) target = floor;
                if (target > newMaxFh) target = newMaxFh;
                if (target !== weatherForecastHour) {
                  weatherForecastHour = target;
                  await windHandle?.setForecastHour(target);
                }
                mapGlobal.map?.render();
              }
            } finally {
              windLoading = false;
            }
          }}
        >
          {#each windModelOptions as m}
            <option value={m.name} disabled={m.disabled} title={m.reason ?? ""}>
              {m.displayName}{m.disabled ? " (n/a)" : ""}
            </option>
          {/each}
        </select>
      </label>
    {/if}
    {#if mapGlobal.layerOptions.find((l) => l.name === "waves")?.on && waveModelOptions.length > 0}
      <label class="wind-forecast-bar-model">
        <span class="wind-forecast-bar-model-prefix">wave</span>
        <select
          disabled={weatherLoading}
          value={waveModel}
          onchange={async (e) => {
            const next = (e.currentTarget as HTMLSelectElement).value;
            waveLoading = true;
            setWeatherError(null);
            try {
              const err = await swapWaveModel(next);
              if (err) {
                // Roll the dropdown back to whatever is actually
                // mounted so the UI doesn't claim a model that
                // isn't rendering.
                setWeatherError(err);
                (e.currentTarget as HTMLSelectElement).value = waveModel;
              }
            } finally {
              waveLoading = false;
            }
          }}
        >
          {#each waveModelOptions as m}
            <option value={m.name} disabled={m.disabled} title={m.reason ?? ""}>
              {m.displayName}{m.disabled ? " (n/a)" : ""}
            </option>
          {/each}
        </select>
      </label>
    {/if}
    <label class="wind-forecast-bar-label">
      {#if previewDate}
        {previewDate.toLocaleString(undefined, {
          weekday: "short",
          month: "short",
          day: "numeric",
          hour: "2-digit",
          minute: "2-digit",
        })}
      {:else}
        +{weatherForecastHour}h
      {/if}
      {#if weatherLoading}
        <span class="wind-forecast-bar-spinner" aria-label="loading" title="loading"></span>
      {/if}
    </label>
    <div class="wind-forecast-bar-slider-wrap">
      {#each dayMarkers as m (m.pct)}
        <span
          class="wind-forecast-bar-daymark"
          style="left: {m.pct}%"
          aria-hidden="true"
        >
          <span class="wind-forecast-bar-daymark-label">{m.label}</span>
        </span>
      {/each}
      <input
        type="range"
        min={weatherMinForecastHour}
        max={weatherMaxForecastHour}
        step="3"
      value={weatherForecastHour}
      disabled={weatherLoading}
      oninput={(e) => {
        const v = parseInt((e.currentTarget as HTMLInputElement).value, 10);
        weatherForecastHour = v;
      }}
      onchange={async (e) => {
        const v = parseInt((e.currentTarget as HTMLInputElement).value, 10);
        // Commit the new value into reactive state *before* awaiting
        // anything. Clicking the slider track (vs dragging) skips
        // `input` in some browsers, so the value prop would otherwise
        // re-render at the stale old value and snap the thumb back
        // to the slider's min.
        weatherForecastHour = v;
        // Sync only the layers the user can actually see. ol-wind
        // setData and ol's GeoJSON readFeatures both block the main
        // thread, so they serialise on the event loop even though
        // we Promise.all them — every off-but-fetched layer adds
        // wall-clock time the user feels as slider lag. The on→off
        // $effect further down catches a layer up the next time
        // it's enabled.
        const weatherParent = mapGlobal.layerOptions.find(
          (l) => l.name === "weather",
        );
        const parentOn = weatherParent?.on ?? true;
        const windOpt = mapGlobal.layerOptions.find((l) => l.name === "wind");
        const waveOpt = mapGlobal.layerOptions.find((l) => l.name === "waves");
        const isobarOpt = mapGlobal.layerOptions.find(
          (l) => l.name === "isobars",
        );
        const syncWind = parentOn && !!windOpt?.on && !!windHandle;
        const syncWave = parentOn && !!waveOpt?.on && !!waveHandle;
        const syncIsobars = parentOn && !!isobarOpt?.on && !!isobarHandle;
        if (syncWind) windLoading = true;
        if (syncWave) waveLoading = true;
        if (syncIsobars) isobarLoading = true;
        // Hide each layer that's about to refetch so the user
        // doesn't see the previous forecast hour's pixels under
        // the new forecast-hour label. Don't touch off layers —
        // their visibility is governed by the panel's $effect.
        const windLayer = windOpt?.layer as any;
        const waveLayer = waveOpt?.layer as any;
        const isobarLayer = isobarOpt?.layer as any;
        if (syncWind) windLayer?.setVisible?.(false);
        if (syncWave) waveLayer?.setVisible?.(false);
        if (syncIsobars) isobarLayer?.setVisible?.(false);
        try {
          const tasks: Promise<void>[] = [];
          if (syncWind && windHandle) tasks.push(windHandle.setForecastHour(v));
          if (syncWave && waveHandle) tasks.push(waveHandle.setForecastHour(v));
          if (syncIsobars && isobarHandle) tasks.push(isobarHandle.setForecastHour(v));
          await Promise.all(tasks);
          if (syncWind && windHandle) {
            weatherForecastHour = windHandle.getForecastHour();
            weatherRunTime = windHandle.getRunTime();
          } else if (syncWave && waveHandle) {
            weatherForecastHour = waveHandle.getForecastHour();
          } else if (syncIsobars && isobarHandle) {
            weatherForecastHour = isobarHandle.getForecastHour();
          } else {
            weatherForecastHour = v;
          }
          mapGlobal.map?.render();
        } finally {
          windLoading = false;
          waveLoading = false;
          isobarLoading = false;
          if (syncWind) windLayer?.setVisible?.(true);
          if (syncWave) waveLayer?.setVisible?.(true);
          if (syncIsobars) isobarLayer?.setVisible?.(true);
        }
      }}
      />
    </div>
    <span class="wind-forecast-bar-runtime">
      {#if weatherRunTime}
        {windModel.toUpperCase()} run {weatherRunTime.slice(0, 16).replace("T", " ")}Z
      {/if}
    </span>
  </div>
  {#if weatherModelError}
    <div class="wind-forecast-bar-error">{weatherModelError}</div>
  {/if}
{/if}

<style>
  .sat-controls {
    position: absolute;
    bottom: 12px;
    left: 50%;
    transform: translateX(-50%);
    z-index: 20;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 6px;
  }
  .sat-band {
    display: flex;
    gap: 2px;
    padding: 3px;
    background: rgba(0, 0, 0, 0.6);
    border-radius: 6px;
  }
  .sat-band-btn {
    padding: 3px 10px;
    border: none;
    border-radius: 4px;
    background: transparent;
    color: #ddd;
    font-size: 12px;
    cursor: pointer;
  }
  .sat-band-btn:hover {
    background: rgba(255, 255, 255, 0.15);
  }
  .sat-band-btn.active {
    background: rgba(255, 255, 255, 0.9);
    color: #000;
    font-weight: 600;
  }
  .sat-time {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 10px;
    background: rgba(0, 0, 0, 0.6);
    border-radius: 6px;
    color: #fff;
    font-size: 12px;
  }
  .sat-time-range {
    width: 180px;
  }
  .sat-time-label {
    min-width: 84px;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .weather-legend-stack {
    position: absolute;
    /* Bottom-left stack — bottom→top: Viam logo (6 px), ScaleLine
       (~50 px), legend stack (80 px). The ScaleLine override below
       lifts OL's distance scale up enough to clear the wordmark
       comfortably. */
    bottom: 80px;
    left: 8px;
    z-index: 10;
    pointer-events: none;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .weather-legend {
    background: rgba(255, 255, 255, 0.9);
    padding: 4px 6px 3px 6px;
    border-radius: 4px;
    border: 1px solid rgba(0, 0, 0, 0.15);
    display: flex;
    flex-direction: column;
    gap: 2px;
    box-shadow: 0 1px 3px rgba(0, 0, 0, 0.18);
    font-size: 10px;
    color: #444;
  }
  .weather-legend-strip {
    display: block;
    width: 200px;
    height: 14px;
    border: 1px solid rgba(0, 0, 0, 0.2);
  }
  /* Mirrors WIND_COLOR_SCALE in src/lib/windLayer.ts: 15 evenly-spaced
     stops from deep blue at 0 kt through teal/green around 10 kt to
     yellow at 16 kt, orange at 20 kt, deep red at 28+ kt. */
  .weather-legend-strip-wind {
    background: linear-gradient(
      to right,
      #0a3d91 0%,
      #1565c0 7.14%,
      #1e88e5 14.29%,
      #4fc3f7 21.43%,
      #26a69a 28.57%,
      #2e7d32 35.71%,
      #66bb6a 42.86%,
      #cddc39 50%,
      #fbc02d 57.14%,
      #f57f17 64.29%,
      #e65100 71.43%,
      #d84315 78.57%,
      #bf360c 85.71%,
      #b71c1c 92.86%,
      #7f0000 100%
    );
  }
  /* Mirrors WAVE_COLOR_SCALE in src/lib/windLayer.ts: near-white at
     calm (so it reads on a blue basemap), cyan around 2 ft, green
     around 4–5 ft, yellow/orange around 6–7 ft, deep red at 10 ft. */
  .weather-legend-strip-wave {
    background: linear-gradient(
      to right,
      #f0f7ff 0%,
      #3eb1ff 20%,
      #3ed24a 40%,
      #bde534 50%,
      #fff200 57%,
      #ff7a1a 70%,
      #e51d1d 85%,
      #6e0606 100%
    );
  }
  .weather-legend-ticks {
    display: flex;
    justify-content: space-between;
    width: 200px;
    line-height: 1;
  }
  .weather-legend-ticks > span {
    white-space: nowrap;
  }

  .wind-forecast-bar {
    position: absolute;
    bottom: 12px;
    left: 50%;
    transform: translateX(-50%);
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 6px 12px;
    background: rgba(255, 255, 255, 0.92);
    border: 1px solid rgba(0, 0, 0, 0.15);
    border-radius: 6px;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.15);
    font-size: 12px;
    color: #333;
    z-index: 10;
    pointer-events: auto;
  }
  .wind-forecast-bar-label {
    font-weight: 600;
    min-width: 70px;
    text-align: right;
  }
  .wind-forecast-bar-spinner {
    display: inline-block;
    width: 12px;
    height: 12px;
    margin-left: 6px;
    vertical-align: -2px;
    border: 2px solid rgba(0, 0, 0, 0.18);
    border-top-color: #0a66c2;
    border-radius: 50%;
    animation: wind-forecast-bar-spin 0.7s linear infinite;
  }
  @keyframes wind-forecast-bar-spin {
    to {
      transform: rotate(360deg);
    }
  }
  .wind-forecast-bar input[type="range"] {
    width: 240px;
    display: block;
  }
  /* Wraps the slider so day-boundary tick marks can be positioned over the
     same width as the input. Padding-top reserves space for the weekday
     labels that sit above the track. */
  .wind-forecast-bar-slider-wrap {
    position: relative;
    width: 240px;
    padding-top: 14px;
  }
  /* One tick per local-midnight forecast hour. The thin vertical bar sits
     inside the slider's track area; the label centres on the same x. The
     ~6 px inset on the wrap is half the native range thumb width so
     ticks line up with the slider's logical 0 % and 100 % positions
     instead of the input element edges. */
  .wind-forecast-bar-daymark {
    position: absolute;
    top: 14px;
    bottom: 0;
    width: 1px;
    margin-left: 6px;
    background: rgba(0, 0, 0, 0.35);
    pointer-events: none;
  }
  .wind-forecast-bar-daymark-label {
    position: absolute;
    bottom: 100%;
    left: 50%;
    transform: translateX(-50%);
    font-size: 10px;
    line-height: 1;
    color: #555;
    white-space: nowrap;
  }
  .wind-forecast-bar-runtime {
    font-size: 11px;
    color: #888;
    min-width: 150px;
  }
  .wind-forecast-bar-model {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    color: #555;
  }
  .wind-forecast-bar-model-prefix {
    color: #888;
    text-transform: uppercase;
    font-size: 10px;
    letter-spacing: 0.04em;
  }
  .wind-forecast-bar-model select {
    font-size: 11px;
    padding: 1px 2px;
    max-width: 160px;
  }
  .wind-forecast-bar-error {
    position: absolute;
    bottom: 48px;
    left: 50%;
    transform: translateX(-50%);
    padding: 4px 10px;
    background: rgba(220, 53, 69, 0.92);
    color: white;
    border-radius: 4px;
    font-size: 11px;
    z-index: 10;
    /* Selectable so the user can copy the upstream error message
       to share / paste into a bug report. The banner sits above
       the slider and ScaleLine but doesn't need to block clicks
       to anything underneath — `auto` only enables selection on
       the banner itself. */
    pointer-events: auto;
    user-select: text;
    -webkit-user-select: text;
    cursor: text;
    max-width: 480px;
    text-align: center;
  }
</style>
