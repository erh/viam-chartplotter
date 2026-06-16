<script lang="ts">
  import { onMount } from "svelte";
  import { getCookie, removeCookie, setCookie } from "typescript-cookie";
  // Importing the logo (rather than referencing /viam-logo.png in public/)
  // routes the URL through Vite's asset pipeline, so it honours the build's
  // `base` and resolves correctly whether the bundle is mounted at "/" or
  // under the Viam Cloud module proxy.
  import viamLogoUrl from "./assets/viam-logo.png";
  import type { BoatInfo, PositionPoint, Detection, DetectionConfig } from "./lib/BoatInfo";
  import { getCountryFromMmsi, flagEmoji } from "./lib/mmsi";
  import RegularShape from "ol/style/RegularShape.js";

  import Collection from "ol/Collection.js";
  import { useGeographic } from "ol/proj.js";
  import { boundingExtent } from "ol/extent.js";
  import {
    WIND_COLOR_SCALE,
    WAVE_COLOR_SCALE,
    colorForValue,
  } from "./lib/windLayer";
  import WeatherOverlays, {
    type WeatherInfo,
    type CursorWeatherSample,
  } from "./lib/WeatherOverlays.svelte";
  import "ol/ol.css";
  import ScaleLine from "ol/control/ScaleLine.js";
  import { defaults as defaultControls } from "ol/control/defaults.js";
  import Map from "ol/Map";
  import View from "ol/View";
  import TileLayer from "ol/layer/Tile";
  import Point from "ol/geom/Point.js";
  import LineString from "ol/geom/LineString.js";
  import GeoJSON from "ol/format/GeoJSON.js";
  import { bbox as bboxStrategy } from "ol/loadingstrategy.js";
  import TileWMS from "ol/source/TileWMS.js";
  import Feature from "ol/Feature.js";
  import VectorSource from "ol/source/Vector.js";
  import { Vector } from "ol/layer.js";
  import XYZ from "ol/source/XYZ";
  import { Circle as CircleStyle, Fill, Icon, Stroke, Style } from "ol/style.js";
  import Overlay from "ol/Overlay.js";
  import { getDistance, offset as sphereOffset } from "ol/sphere.js";
  import Modify from "ol/interaction/Modify.js";
  import MouseWheelZoom from "ol/interaction/MouseWheelZoom.js";
  import type { Geometry } from "ol/geom";
  import type BaseLayer from "ol/layer/Base";

  // ol-wind (sakitam-fdd/wind-layer's OpenLayers build) — optional
  // weather overlay. Dynamic-imported so the chartplotter still loads
  // cleanly if the package isn't installed (`npm i ol-wind`) or wind
  // data isn't reachable.

  interface LayerOption {
    name: string;
    displayName?: string; // Optional display name for UI (defaults to name)
    on: boolean;
    // Optional: virtual entries (e.g. ais-projection) appear in the
    // layers panel as toggles but don't correspond to a real OL layer —
    // their style is rendered inline by another layer's style function.
    layer?: TileLayer<any> | Vector<any> | BaseLayer;
    parent?: string; // Parent layer name for hierarchical layers
    // Optional zoom gate: when the current view zoom is below this
    // value the layer is hidden (setVisible(false)) regardless of the
    // user toggle. Used for vector layers whose icons would clutter
    // at overview scales — navaids, structures, etc. Leave undefined
    // for layers that should always be visible when toggled on.
    minZoom?: number;
    // Inverse zoom gate: hidden when zoom > maxZoom. Used by overlays
    // whose data resolution stops being useful when zoomed in past a
    // certain point — e.g. GFS 0.25° wind / wave at chart-detail zoom,
    // where one model cell spans hundreds of tile pixels and the
    // particle field becomes a flat coloured wash.
    maxZoom?: number;
  }

  // Constants used by the cursor wind/wave readout. ol-wind's Field
  // carries the magnitude slot — wave height in m for the wave layer.
  // WAVE_RANGE_MAX_M / METERS_TO_FEET / MS_TO_KT are shared with the
  // (non-weather) cursor readout markup below; the weather overlay UI
  // keeps its own copies in WeatherOverlays.svelte.
  const WAVE_RANGE_MAX_M = 3;
  const METERS_TO_FEET = 3.28084;
  const MS_TO_KT = 1.94384;
  // Reactive copy of map zoom so template conditionals (slider / wave
  // legend / etc.) re-render when the user scrolls past the gate. The
  // resolution-change listener installed in setupMap writes here.
  let currentZoom = $state(0);

  // Chart base layers (radio-exclusive). When one is the active base, the
  // opaque chart covers US waters, so the under-chart OSM fallback is wasted
  // there.
  const CHART_BASE_NAMES = ["checkmate", "noaa", "noaa-ecdis"];
  // Tracks the last chartBaseActive() so updateOnLayers can refresh the OSM
  // base source when the active base flips (the tileUrlFunction skips US tiles
  // only while a chart base is active, so switching bases must re-evaluate).
  let prevChartBaseActive = true;
  function chartBaseActive(): boolean {
    return CHART_BASE_NAMES.some((n) => {
      const o = mapGlobal.layerOptions.find((p) => p.name === n);
      return !!o && o.on;
    });
  }

  // Approximate NOAA ENC coverage (US marine waters). Used to skip the
  // under-chart OSM fallback fetch where the chart already covers the tile —
  // see the OSM base tileUrlFunction. Generous rectangles; tiles only partly
  // inside (coverage edges) still load OSM, so no blank gaps at the boundary.
  const US_ENC_COVERAGE: Array<[number, number, number, number]> = [
    [-128, 22, -64, 50], // CONUS + Atlantic/Gulf/Pacific coasts + Great Lakes
    [-180, 50, -128, 73], // Alaska (mainland + eastern Aleutians)
    [172, 50, 180, 73], //   Alaska (Aleutians across the dateline)
    [-161, 18, -154, 23], // Hawaii
    [-68.2, 17.3, -64.2, 19.1], // Puerto Rico / USVI
    [144, 13, 146.5, 21], // Guam / CNMI
    [-171.5, -14.6, -168, -10.8], // American Samoa
  ];
  // True when the whole XYZ tile falls inside US ENC coverage (web-mercator
  // tile → lon/lat box, fully-contained test).
  function tileFullyInUSWaters(z: number, x: number, y: number): boolean {
    const n = 2 ** z;
    const lonW = (x / n) * 360 - 180;
    const lonE = ((x + 1) / n) * 360 - 180;
    const latN =
      (Math.atan(Math.sinh(Math.PI * (1 - (2 * y) / n))) * 180) / Math.PI;
    const latS =
      (Math.atan(Math.sinh(Math.PI * (1 - (2 * (y + 1)) / n))) * 180) / Math.PI;
    return US_ENC_COVERAGE.some(
      (b) => lonW >= b[0] && lonE <= b[2] && latS >= b[1] && latN <= b[3],
    );
  }

  let boatImage = "topdown-boat.svg";

  // myBoat-only icon override. The Go module exposes /myboat-icon when the
  // operator sets `myboat_icon_path` in config; we probe once on mount and
  // swap in that URL just for the user's-own-boat marker. AIS markers keep
  // using the bundled boatImage above.
  let myBoatImage = $state<string>(boatImage);
  // Natural pixel dimensions of the override icon, captured after preload.
  // The createBoatStyle scale was tuned to topdown-boat.svg's 24x73; we
  // remap by the height ratio so a configured override renders at the same
  // pixel size as the bundled icon regardless of the override's resolution.
  const BOAT_IMAGE_NATURAL_HEIGHT = 73;

  // Boat icons scale relative to an 80 ft "default" vessel (~6 m beam).
  // Length scales the icon's long axis; the cross-axis only scales when we
  // have a beam value, otherwise it stays at the default width (so an 800 ft
  // tanker just looks longer, not also wider, when no beam is reported).
  // sqrt curve dampens the scaling so a 200 ft boat is ~1.6x not 2.5x.
  const DEFAULT_BOAT_LENGTH_M = 24.38; // 80 ft
  const DEFAULT_BOAT_BEAM_M = 6.0; // typical 80 ft motoryacht beam
  const BOAT_SCALE_MIN = 0.6;
  const BOAT_SCALE_MAX = 2.5;

  function dimScaleFactor(
    valueMeters: number | null | undefined,
    referenceMeters: number
  ): number {
    if (!valueMeters || !Number.isFinite(valueMeters) || valueMeters <= 0) {
      return 1;
    }
    const f = Math.sqrt(valueMeters / referenceMeters);
    return Math.max(BOAT_SCALE_MIN, Math.min(BOAT_SCALE_MAX, f));
  }

  // Returns [scaleX, scaleY] — X is across the boat (beam), Y is along (length).
  // When beam is unknown we leave X at 1 so only the long axis grows.
  function boatScaleAxes(
    lengthMeters: number | null | undefined,
    beamMeters: number | null | undefined
  ): [number, number] {
    const sy = dimScaleFactor(lengthMeters, DEFAULT_BOAT_LENGTH_M);
    const sx = beamMeters
      ? dimScaleFactor(beamMeters, DEFAULT_BOAT_BEAM_M)
      : 1;
    return [sx, sy];
  }
  // Floor on rendered width for the override icon. Some PNGs are very
  // narrow (tall thin silhouette) and the height-ratio remap can leave
  // them only a few pixels wide on screen — too small to spot on a busy
  // chart. Bump scale up to guarantee at least this many pixels of width.
  const MYBOAT_MIN_RENDERED_WIDTH_PX = 20;
  let myBoatImageNaturalWidth = $state<number | null>(null);
  let myBoatImageNaturalHeight = $state<number | null>(null);

  // Base maps are mutually exclusive — only one can be active at a time.
  // The layer panel renders these (and their children) above a divider as
  // radio buttons; everything else (boat, ais, airstream + their children)
  // renders below as independent checkboxes.
  const BASE_LAYER_NAMES = ["open street map", "noaa", "checkmate", "noaa-ecdis"];
  function isBaseLayerGroup(l: { name: string; parent?: string }): boolean {
    return (
      BASE_LAYER_NAMES.includes(l.name) ||
      (!!l.parent && BASE_LAYER_NAMES.includes(l.parent))
    );
  }

  let popupState = $state({
    overlay: null as Overlay | null,
    visible: false,
    content: {
      name: "",
      mmsi: "",
      speed: 0,
      heading: 0,
      cog: null as number | null | undefined,
      lat: 0,
      lng: 0,
      isMyBoat: false,
      host: undefined as string | undefined,
      partId: undefined as string | undefined,
      isOnline: true,
      length: undefined as number | undefined,
      destination: undefined as string | undefined,
      cpaNm: null as number | null,
      tcpaMin: null as number | null,
    },
  });

  // Country flag for the popup title — derived from the popup target's
  // MMSI (empty for myBoat, populated for AIS clicks). Re-evaluates
  // automatically when the user clicks a different vessel.
  const popupCountry = $derived(getCountryFromMmsi(popupState.content.mmsi));

  // Popup shown when the user clicks a waypoint or a route segment in edit
  // mode. Mode "insert" offers "add waypoint here" between two existing
  // waypoints; mode "delete" offers "delete this waypoint" for the clicked
  // marker.
  let editPopupState = $state({
    overlay: null as Overlay | null,
    visible: false,
    mode: "insert" as "insert" | "delete",
    lat: 0,
    lng: 0,
    // For "insert": the waypoint to slot before. For "delete": the waypoint to remove.
    waypointId: "",
  });

  let layersExpanded = $state(false);
  let boatsExpanded = $state(false);
  let mapLoaded = $state(false);
  let currentDetections = $state<Detection[] | undefined>(undefined);
  let boatSearchTerm = $state("");

  let measureActive = $state(false);
  let measurePoints = $state<number[][]>([]);
  let measureDistance = $state<number | null>(null);
  let measureSource: VectorSource | null = null;

  // Debug helper: when on, clicking the map prints + copies the checkmate
  // tile URL covering that point. Used for diffing our render against NOAA's
  // WMS for a specific tile.
  let tileUrlActive = $state(false);

  let addWaypointActive = $state(false);

  const COOKIE_HEADS_UP = "mapHeadsUp";
  const COOKIE_LAYERS = "mapLayers";
  const COOKIE_HEADING_LINE_LENGTH = "mapHeadingLineLengthNm";
  const COOKIE_AIS_PROJECTION_MIN = "mapAisProjectionMin";
  const COOKIE_BOAT_POSITION = "mapBoatPosition";
  const COOKIE_AUTO_ZOOM = "mapAutoZoom";
  const COOKIE_VIEW_ZOOM = "mapViewZoom";
  // Persisted only while inPanMode: the user's manual pan position, so a
  // reload lands them back where they were instead of jumping to the boat.
  // Cleared when the user returns to boat-follow mode.
  const COOKIE_VIEW_CENTER = "mapViewCenter";
  const COOKIE_OPTS = { expires: 365, sameSite: "lax" as const, path: "/" };

  // Default view when no cookie is present: centred between NYC and
  // Hudson Canyon (~40°N, 73°W in user coords because we use
  // useGeographic()) at a zoom that keeps both on screen.
  const DEFAULT_VIEW_ZOOM = 7;
  const DEFAULT_VIEW_CENTER: [number, number] = [-73.0, 40.0];

  function loadViewZoom(): number {
    var raw = getCookie(COOKIE_VIEW_ZOOM);
    if (!raw) return DEFAULT_VIEW_ZOOM;
    var n = Number(raw);
    return Number.isFinite(n) && n > 0 && n <= 22 ? n : DEFAULT_VIEW_ZOOM;
  }

  function loadViewCenter(): [number, number] | null {
    var raw = getCookie(COOKIE_VIEW_CENTER);
    if (!raw) return null;
    try {
      var parsed = JSON.parse(raw);
      if (
        Array.isArray(parsed) &&
        parsed.length === 2 &&
        Number.isFinite(parsed[0]) &&
        Number.isFinite(parsed[1])
      ) {
        return [parsed[0], parsed[1]];
      }
    } catch {
      // fall through
    }
    return null;
  }

  const HEADING_LINE_LENGTH_OPTIONS = [1, 2, 3, 5, 10, 15];
  const AIS_PROJECTION_OPTIONS = [1, 2, 5, 10];

  // Cache-busting tile version. Appended as a `v=` query param on every tile
  // URL. Default is the build-time git short hash (injected by Vite via the
  // __GIT_HASH__ define) so every new build auto-busts OpenLayers and the
  // browser HTTP cache without manual intervention. Override per page load
  // via `?tilev=foo` to force a one-off bust.
  const tileGenVersion: string = (() => {
    const fallback = typeof __GIT_HASH__ === "string" ? __GIT_HASH__ : "dev";
    if (typeof window === "undefined") return fallback;
    try {
      const v = new URLSearchParams(window.location.search).get("tilev");
      return v && v !== "" ? v : fallback;
    } catch {
      return fallback;
    }
  })();

  // Boat safety depth (feet). Drives the DEPARE gradient on local NOAA-ENC
  // tiles: solid coral below this, gradient to white at 2× this. Override per
  // page load via `?safeDepth=N`; otherwise the server uses its module default
  // (`safe_depth_ft` config attribute).
  const safeDepthParam: string = (() => {
    if (typeof window === "undefined") return "";
    try {
      const v = new URLSearchParams(window.location.search).get("safeDepth");
      return v && v !== "" ? v : "";
    } catch {
      return "";
    }
  })();

  let headsUpActive = $state(getCookie(COOKIE_HEADS_UP) === "1");
  // boat position on screen: "center" or "bottom" (~80% down from top)
  let boatPositionMode = $state<"center" | "bottom">(
    getCookie(COOKIE_BOAT_POSITION) === "bottom" ? "bottom" : "center"
  );
  // Auto-zoom: when on, recenter ticks override the user's zoom with a
  // speed-derived value. Default off so the user keeps full control unless
  // they opt in.
  let autoZoomActive = $state(getCookie(COOKIE_AUTO_ZOOM) === "1");

  function loadHeadingLineLength(): number {
    var raw = getCookie(COOKIE_HEADING_LINE_LENGTH);
    var parsed = raw ? Number(raw) : NaN;
    return HEADING_LINE_LENGTH_OPTIONS.includes(parsed) ? parsed : 5;
  }
  let headingLineLengthNm = $state(loadHeadingLineLength());

  function setHeadingLineLength(nm: number) {
    headingLineLengthNm = nm;
    setCookie(COOKIE_HEADING_LINE_LENGTH, String(nm), COOKIE_OPTS);
    updateHeadingLine();
  }

  function loadAisProjectionMin(): number {
    var raw = getCookie(COOKIE_AIS_PROJECTION_MIN);
    var parsed = raw ? Number(raw) : NaN;
    return AIS_PROJECTION_OPTIONS.includes(parsed) ? parsed : 2;
  }
  let aisProjectionMinutes = $state(loadAisProjectionMin());

  function setAisProjectionMinutes(min: number) {
    aisProjectionMinutes = min;
    setCookie(COOKIE_AIS_PROJECTION_MIN, String(min), COOKIE_OPTS);
    // Force the AIS layer to redraw so the new projection length takes
    // effect immediately. OL caches feature renders until told otherwise.
    mapGlobal.aisLayer?.changed();
  }

  function loadSavedLayerStates(): Record<string, boolean> {
    var raw = getCookie(COOKIE_LAYERS);
    if (!raw) return {};
    try {
      var parsed = JSON.parse(raw);
      return typeof parsed === "object" && parsed !== null ? parsed : {};
    } catch {
      return {};
    }
  }

  function saveLayerStates() {
    var states: Record<string, boolean> = {};
    for (var l of mapGlobal.layerOptions) {
      states[l.name] = l.on;
    }
    setCookie(COOKIE_LAYERS, JSON.stringify(states), COOKIE_OPTS);
  }

  // Track which boats are visible (by mmsi, plus 'myBoat' for own boat)
  // When externalVisibilityControl is true, start with empty set (parent will control)
  let visibleBoats = $state<Set<string>>(new Set(["myBoat"]));

  // Effective visible boats: filters visibleBoats by search term
  // Boats not matching search are hidden on map AND their tracking layers
  const effectiveVisibleBoats = $derived.by(() => {
    if (!boatSearchTerm.trim()) return visibleBoats;
    const searchLower = boatSearchTerm.toLowerCase();
    const filtered = new Set<string>();
    visibleBoats.forEach((id) => {
      if (id === "myBoat") {
        filtered.add(id); // Always show myBoat if checked
        return;
      }
      const boat = boats?.find((b) => b.mmsi === id);
      if (
        boat &&
        (boat.name.toLowerCase().includes(searchLower) ||
          boat.mmsi?.toLowerCase().includes(searchLower))
      ) {
        filtered.add(id);
      }
    });
    return filtered;
  });

  // Initialize visibleBoats when boats prop changes (only when NOT using external control)
  // Use plain JS variable for tracking to avoid creating effect dependencies
  let lastBoatsLength = 0;
  $effect(() => {
    // Skip auto-add when parent is controlling visibility externally
    if (externalVisibilityControl) return;

    // Read boats to create dependency
    const boatsList = boats;
    const currentLength = boatsList?.length ?? 0;

    if (currentLength !== lastBoatsLength) {
      // Add any new boats to visible set
      boatsList?.forEach((b) => {
        if (b.mmsi && !visibleBoats.has(b.mmsi)) {
          visibleBoats.add(b.mmsi);
        }
      });
      lastBoatsLength = currentLength; // Plain JS variable, won't re-trigger
      visibleBoats = new Set(visibleBoats); // Trigger reactivity
    }
  });

  function toggleBoatVisibility(id: string) {
    if (visibleBoats.has(id)) {
      visibleBoats.delete(id);
    } else {
      visibleBoats.add(id);
    }
    visibleBoats = new Set(visibleBoats); // Trigger reactivity
  }

  function selectAllBoats() {
    const allIds = new Set<string>();
    if (myBoat) allIds.add("myBoat");
    boats?.forEach((b) => {
      // Only add offline boats if showOfflineBoatsInPanel is true
      if (b.mmsi && (b.isOnline !== false || showOfflineBoatsInPanel)) {
        allIds.add(b.mmsi);
      }
    });
    visibleBoats = allIds;
  }

  function deselectAllBoats() {
    visibleBoats = new Set();
  }

  // Set which boats are visible by Set of mmsi/ids (for external control)
  function setVisibleBoats(ids: Set<string>) {
    visibleBoats = ids;
  }

  function isValidCoordinate(lat: number, lng: number): boolean {
    return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180 && !(lat === 0 && lng === 0); // Exclude null island
  }

  // Initial bearing from `from` to `to` in degrees [0, 360). Standard
  // forward-azimuth formula; we don't need accuracy beyond what the user
  // would steer to.
  function bearingDeg(from: [number, number], to: [number, number]): number {
    const toRad = (d: number) => (d * Math.PI) / 180;
    const φ1 = toRad(from[0]);
    const φ2 = toRad(to[0]);
    const Δλ = toRad(to[1] - from[1]);
    const y = Math.sin(Δλ) * Math.cos(φ2);
    const x = Math.cos(φ1) * Math.sin(φ2) - Math.sin(φ1) * Math.cos(φ2) * Math.cos(Δλ);
    return ((Math.atan2(y, x) * 180) / Math.PI + 360) % 360;
  }

  // Hover-time tooltip formatter. Shows time-of-day if the segment is
  // from today, otherwise prepends a short date so the user can tell at
  // a glance whether they're looking at this morning or last week.
  function formatTrackTime(ts: number): string {
    const d = new Date(ts);
    const now = new Date();
    const sameDay =
      d.getFullYear() === now.getFullYear() &&
      d.getMonth() === now.getMonth() &&
      d.getDate() === now.getDate();
    const time = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
    if (sameDay) return time;
    const date = d.toLocaleDateString([], { month: "short", day: "numeric" });
    return `${date} ${time}`;
  }

  function formatDurationMin(min: number): string {
    if (!isFinite(min) || min <= 0) return "—";
    if (min < 60) return `${Math.round(min)} min`;
    const h = Math.floor(min / 60);
    const m = Math.round(min % 60);
    return m === 0 ? `${h}h` : `${h}h ${m}m`;
  }

  function formatEta(min: number): string {
    if (!isFinite(min) || min <= 0) return "—";
    const eta = new Date(Date.now() + min * 60000);
    return eta.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }

  // Derived overlay stats for the active route. Recomputes whenever the boat
  // pose or waypoint list changes.
  let routeStats = $derived.by(() => {
    if (!navWaypoints || navWaypoints.length === 0 || !myBoat) return null;
    if (!isValidCoordinate(myBoat.location[0], myBoat.location[1])) return null;

    const speedKn = myBoat.speed ?? 0;
    const startLngLat: [number, number] = [myBoat.location[1], myBoat.location[0]];

    const next = navWaypoints[0];
    const nextLngLat: [number, number] = [next.lng, next.lat];
    const nextMeters = getDistance(startLngLat, nextLngLat);
    const nextNm = nextMeters / 1852;
    const nextBearing = bearingDeg(
      [myBoat.location[0], myBoat.location[1]],
      [next.lat, next.lng]
    );
    // hours = nm / kn ; convert to minutes
    const nextMin = speedKn > 0.1 ? (nextNm / speedKn) * 60 : Infinity;

    let totalMeters = nextMeters;
    let prev = nextLngLat;
    for (let i = 1; i < navWaypoints.length; i++) {
      const cur: [number, number] = [navWaypoints[i].lng, navWaypoints[i].lat];
      totalMeters += getDistance(prev, cur);
      prev = cur;
    }
    const totalNm = totalMeters / 1852;
    const totalMin = speedKn > 0.1 ? (totalNm / speedKn) * 60 : Infinity;

    return {
      next: {
        distNm: nextNm,
        headingDeg: nextBearing,
        minutes: nextMin,
      },
      final: {
        distNm: totalNm,
        minutes: totalMin,
        waypointCount: navWaypoints.length,
      },
    };
  });

  function fitToVisibleBoats() {
    if (!mapGlobal.map || !mapGlobal.view) return;

    const coords: number[][] = [];

    // Add my boat if visible and has valid coordinates
    if (
      myBoat &&
      effectiveVisibleBoats.has("myBoat") &&
      isValidCoordinate(myBoat.location[0], myBoat.location[1])
    ) {
      coords.push([myBoat.location[1], myBoat.location[0]]); // [lng, lat]
    }

    // Add visible AIS boats with valid coordinates
    boats?.forEach((boat) => {
      if (
        boat.mmsi &&
        effectiveVisibleBoats.has(boat.mmsi) &&
        isValidCoordinate(boat.location[0], boat.location[1])
      ) {
        coords.push([boat.location[1], boat.location[0]]);
      }
    });

    if (coords.length === 0) return;

    if (coords.length === 1) {
      // Single boat - center on it with reasonable zoom
      mapGlobal.view.animate({
        center: coords[0],
        zoom: Math.min(12, Math.max(8, mapGlobal.view.getZoom() ?? 10)),
        duration: 500,
      });
    } else {
      // Multiple boats - fit to extent with responsive padding
      const extent = boundingExtent(coords);
      const mapSize = mapGlobal.map?.getSize() || [800, 600];
      const [width, height] = mapSize;

      // Calculate default padding values
      const defaultHorizontalPad = Math.max(80, width * 0.1);
      const defaultVerticalPad = Math.max(80, height * 0.1);
      const defaultTopPad = Math.max(250, height * 0.15); // Extra top padding for 223px popup height

      // Apply custom padding if provided, otherwise use defaults
      let topPad: number, rightPad: number, bottomPad: number, leftPad: number;

      if (typeof fitBoundsPadding === "number") {
        // Single number applies to all edges
        topPad = rightPad = bottomPad = leftPad = fitBoundsPadding;
      } else if (fitBoundsPadding) {
        // Object with individual edge overrides
        topPad = fitBoundsPadding.top ?? defaultTopPad;
        rightPad = fitBoundsPadding.right ?? defaultHorizontalPad;
        bottomPad = fitBoundsPadding.bottom ?? defaultVerticalPad;
        leftPad = fitBoundsPadding.left ?? defaultHorizontalPad;
      } else {
        // Use all defaults
        topPad = defaultTopPad;
        rightPad = defaultHorizontalPad;
        bottomPad = defaultVerticalPad;
        leftPad = defaultHorizontalPad;
      }

      mapGlobal.view.fit(extent, {
        padding: [topPad, rightPad, bottomPad, leftPad],
        duration: 500,
        maxZoom: 12,
      });
    }

    inPanMode = true; // Prevent auto-centering
  }

  let {
    myBoat,
    zoomModifier,
    boats,
    positionHistorical,
    // $bindable so the in-map layers panel can flip the toggle and the
    // parent (App.svelte) sees the change reactively.
    depthColorTrack = $bindable(false),
    depthSensorAvailable = false,
    enableBoatsPanel = false,
    externalVisibilityControl = false,
    showOfflineBoatsInPanel = true,
    defaultAisVisible = true,
    fullWidth = false,
    chartOnly = false,
    navWaypoints,
    onAddWaypoint,
    onMoveWaypoint,
    onInsertWaypoint,
    onRemoveWaypoint,
    onClearWaypoints,
    onReady,
    boatDetailSlot,
    fitBoundsPadding,
    onBoatPopupOpen,
    detectionConfig,
    airstreamConfigured = false,
    onAirstreamBboxChange,
    sog,
    hdg,
    cog,
    depth,
    aisTracksNeeded = $bindable(false),
  }: {
    myBoat?: BoatInfo;
    zoomModifier?: number;
    boats?: BoatInfo[];
    positionHistorical?: PositionPoint[];
    depthColorTrack?: boolean;
    /** True when a depth sensor is configured. Gates whether the
     *  "color track by depth" toggle is shown in the layers panel. */
    depthSensorAvailable?: boolean;
    enableBoatsPanel?: boolean;
    fullWidth?: boolean;
    /** Chart-extended (kiosk) mode: no boat/robot — hide the map toolbar and
     *  controls and show only the chart. */
    chartOnly?: boolean;
    /** Speed-over-ground (knots). When provided, shown in the combined data
     *  panel at the top-right of the map. */
    sog?: number | null;
    /** Compass heading (degrees). When provided, shown in the data panel. */
    hdg?: number | null;
    /** Course-over-ground (degrees). When provided, shown in the data panel. */
    cog?: number | null;
    /** Water depth (ft). When provided, shown in the data panel. */
    depth?: number | null;
    /** Bindable; reflects whether the AIS-track layer (and its parent
     *  AIS layer) are both currently visible. Parent reads this to
     *  decide whether to keep fetching per-vessel AIS history — when
     *  the user has the layer off, the history poll is wasted work. */
    aisTracksNeeded?: boolean;
    /** Ordered waypoints from a navigation service. The route is drawn from the boat's
     *  current position through each waypoint in order. */
    navWaypoints?: { id: string; lat: number; lng: number }[];
    /** Called when the user clicks the map while "add waypoint" mode is active. */
    onAddWaypoint?: (lat: number, lng: number) => void;
    /** Called when the user finishes dragging an existing waypoint. */
    onMoveWaypoint?: (id: string, lat: number, lng: number) => void;
    /** Called when the user picks "add waypoint here" on a route segment.
     *  beforeId is the ID of the waypoint the new one should sort before;
     *  empty string means "append to the end of the route". */
    onInsertWaypoint?: (beforeId: string, lat: number, lng: number) => void;
    /** Called when the user picks "delete this waypoint" on an existing marker. */
    onRemoveWaypoint?: (id: string) => void;
    /** Called when the user clicks the clear-route button. */
    onClearWaypoints?: () => void;
    /** When true, parent controls visibility via setVisibleBoats API instead of auto-showing new boats */
    externalVisibilityControl?: boolean;
    /** When false, offline boats are hidden from the boats panel (default: true) */
    showOfflineBoatsInPanel?: boolean;
    defaultAisVisible?: boolean;
    onReady?: (api: {
      fitToVisibleBoats: () => void;
      selectAllBoats: () => void;
      deselectAllBoats: () => void;
      setVisibleBoats: (ids: Set<string>) => void;
      getVisibleBoats: () => Set<string>;
      setDetections: (detections: Detection[] | undefined) => void;
      /** Focus a boat by mmsi/partId: make it visible (even if offline), fly to it, and open its popup. */
      focusBoat: (mmsi: string) => void;
    }) => void;
    boatDetailSlot?: (boat: { host?: string; partId?: string; name: string }) => any;
    fitBoundsPadding?: number | { top?: number; right?: number; bottom?: number; left?: number };
    onBoatPopupOpen?: (boatPartId?: string) => void;
    detectionConfig?: DetectionConfig;
    /** When true, register the airstream toggle layer in the layer panel.
     *  When false (default) it's hidden — the host machine has no airstream
     *  component to drive. */
    airstreamConfigured?: boolean;
    /** Called by the map when the airstream layer's bbox changes: bbox=null
     *  means the layer was toggled off, otherwise bbox is the current
     *  viewport in lon/lat. App.svelte uses this to hit airstream's DoCommand
     *  (and to gate fetching from it). */
    onAirstreamBboxChange?: (
      bbox: { minLon: number; minLat: number; maxLon: number; maxLat: number } | null
    ) => void;
  } = $props();

  // Create derived values for reactivity tracking
  let boatsKey = $derived(
    JSON.stringify(boats?.map((b) => [b.location, b.speed, b.heading, b.positionHistory?.length]))
  );
  let myBoatKey = $derived(
    myBoat ? JSON.stringify([myBoat.heading, myBoat.location, myBoat.speed, myBoat.route]) : null
  );
  let navWaypointsKey = $derived(JSON.stringify(navWaypoints ?? []));
  let visibleBoatsKey = $derived(JSON.stringify([...visibleBoats]));
  let effectiveVisibleKey = $derived(JSON.stringify([...effectiveVisibleBoats]));

  $effect(() => {
    // Read derived keys to create dependencies
    const _boats = boatsKey;
    const _myBoat = myBoatKey;
    const _visible = visibleBoatsKey;
    const _wps = navWaypointsKey;
    updateFromData();
  });

  // Update popup content if it's open and showing a boat that moved
  $effect(() => {
    if (!popupState.visible) return;

    if (popupState.content.isMyBoat && myBoat) {
      // Update my boat popup
      popupState.content.speed = myBoat.speed;
      popupState.content.heading = myBoat.heading;
      popupState.content.cog = myBoat.cog;
      popupState.content.lat = myBoat.location[0];
      popupState.content.lng = myBoat.location[1];
    } else if (popupState.content.mmsi && boats) {
      // Update AIS boat popup
      const boat = boats.find((b) => b.mmsi === popupState.content.mmsi);
      if (boat) {
        popupState.content.speed = boat.speed;
        popupState.content.heading = boat.heading;
        popupState.content.cog = boat.cog;
        popupState.content.lat = boat.location[0];
        popupState.content.lng = boat.location[1];
        popupState.content.length = boat.length;
        popupState.content.destination = boat.destination;
        if (
          myBoat &&
          myBoat.location &&
          !(myBoat.location[0] === 0 && myBoat.location[1] === 0)
        ) {
          const r = computeCpa(
            myBoat.location[0],
            myBoat.location[1],
            myBoat.heading,
            myBoat.speed,
            boat.location[0],
            boat.location[1],
            boat.cog ?? boat.heading,
            boat.speed
          );
          popupState.content.cpaNm = r ? r.cpaNm : null;
          popupState.content.tcpaMin = r ? r.tcpaMin : null;
        } else {
          popupState.content.cpaNm = null;
          popupState.content.tcpaMin = null;
        }
      }
    }
  });

  // Close popup if the displayed boat gets hidden or removed
  $effect(() => {
    if (!popupState.visible) return;

    visibleBoatsKey; // Track visibility changes
    boatsKey; // Track boats array changes

    const shouldClose = popupState.content.isMyBoat
      ? !visibleBoats.has("myBoat")
      : popupState.content.mmsi &&
        (!boats?.some((b) => b.mmsi === popupState.content.mmsi) ||
          !visibleBoats.has(popupState.content.mmsi));

    if (shouldClose) closePopup();
  });

  $effect(() => {
    const _visible = effectiveVisibleKey;
    const _depthColor = depthColorTrack;
    // Re-style when the popup opens/closes or switches target so the
    // "show only the selected boat's track" filter takes effect.
    const _popupVisible = popupState.visible;
    const _popupMmsi = popupState.content.mmsi;
    const _popupIsMyBoat = popupState.content.isMyBoat;
    if (mapGlobal.trackLayer) {
      mapGlobal.trackLayer.getSource()?.changed();
    }
    if (mapGlobal.aisTrackLayer) {
      mapGlobal.aisTrackLayer.getSource()?.changed();
    }
  });

  // Sync layer visibility when layer options change
  $effect(() => {
    // Read all layer states to create dependencies
    const states = mapGlobal.layerOptions.map((l) => ({ name: l.name, on: l.on }));
    // Re-run when the popup opens/closes on an AIS boat — that case force-
    // shows the AIS-track layer (see updateOnLayers) so the selected boat's
    // history appears even with the user's track toggle off.
    const _popupVisible = popupState.visible;
    const _popupMmsi = popupState.content.mmsi;
    const _popupIsMyBoat = popupState.content.isMyBoat;
    // Re-render AIS when its projection-line toggle flips. ais-projection
    // is a virtual layer (no OL layer attached) — toggling it doesn't
    // hit updateOnLayers' add/remove path, so we have to nudge the AIS
    // layer ourselves so its style function is re-evaluated.
    void states.find((s) => s.name === "ais-projection")?.on;
    mapGlobal.aisLayer?.changed();

    // Surface "do we need AIS history?" to the parent so it can stop
    // polling all_history when the user has the track layer turned
    // off. Both parents must be on — toggling the umbrella "ais" off
    // hides the tracks even if the child "ais-track" is on.
    const aisOn = states.find((s) => s.name === "ais")?.on ?? false;
    const aisTrackOn = states.find((s) => s.name === "ais-track")?.on ?? false;
    aisTracksNeeded = aisOn && aisTrackOn;

    updateOnLayers();
  });

  $effect(() => {
    if (mapLoaded) {
      renderDetections(currentDetections);
    }
  });

  let mapGlobal = $state({
    map: null as Map | null,
    view: null as View | null,

    aisFeatures: new Collection<Feature<Geometry>>(),
    trackFeatures: new Collection<Feature<Geometry>>(),
    aisTrackFeatures: new Collection<Feature<Geometry>>(),
    routeFeatures: new Collection<Feature<Geometry>>(),
    navRouteLineFeatures: new Collection<Feature<Geometry>>(),
    navWaypointFeatures: new Collection<Feature<Geometry>>(),
    headingLineFeatures: new Collection<Feature<Geometry>>(),
    trackFeaturesLastCheck: new Date(0),
    myBoatMarker: null as Feature<Point> | null,

    // Track layer references for refreshing styles
    trackLayer: null as Vector<any> | null,
    aisLayer: null as Vector<any> | null,
    aisTrackLayer: null as Vector<any> | null,
    navaidLayer: null as Vector<any> | null,
    structureLayer: null as Vector<any> | null,

    layerOptions: [] as LayerOption[],
    onLayers: new Collection<BaseLayer>(),
  });

  let inPanMode = $state(false);

  // Cursor-info: distance + bearing from boat to mouse pointer. null when
  // the pointer isn't over the map or when there's no usable boat fix.
  // Rendered as a fixed-position box in the bottom-left of the map (above
  // the scale line) so it doesn't move around as the user moves the mouse.
  let cursorInfo = $state<{
    lat: number;
    lng: number;
    nm: number | null;
    brg: number | null;
    windKt: number | null;
    windFromDeg: number | null;
    waveM: number | null;
    waveFromDeg: number | null;
  } | null>(null);

  let mapInternalState: {
    lastZoom: number;
    lastCenter: number[] | null;
    lastPosition: number[];
    lastPositions: Record<string, number[]>;
    trackFeatureIds: Record<string, boolean>;
    aisTrackFeatureIds: Record<string, boolean>;
    lastPosHistoricalKey: string;
    // Timestamps (ms) of realtime track points we've actually recorded,
    // per boat. Used by renderHistoricalTrack to avoid double-painting
    // the last-10-minute window when realtime already has it covered,
    // while still painting historical wherever realtime has a gap.
    realtimeTrackTs: Record<string, number[]>;
  } = {
    lastZoom: 0,
    lastCenter: null,
    lastPosition: [0, 0],
    lastPositions: {},
    trackFeatureIds: {},
    aisTrackFeatureIds: {},
    lastPosHistoricalKey: "",
    realtimeTrackTs: {},
  };

  // Realtime "wins" for the last realtimeWindowMs ms; historical paints
  // anything older. Within the window, historical only paints where
  // realtime has a gap larger than realtimeMatchToleranceMs.
  const realtimeWindowMs = 10 * 60 * 1000;
  const realtimeMatchToleranceMs = 30 * 1000;
  // Cap how far back we keep realtime timestamps. Anything past the
  // realtime window plus a margin can be dropped — historical takes
  // over there anyway.
  const realtimeTsKeepMs = realtimeWindowMs + 5 * 60 * 1000;

  function updateFromData() {
    if (!mapGlobal.map || !mapGlobal.view) {
      return;
    }

    // Pan-mode detection is now exclusively via the pointerdrag handler
    // (5 px threshold). The previous diff-based check on view.getCenter()
    // tripped on programmatic center shifts that the boat-anchored
    // wheel-zoom necessarily produces in "bottom" mode (boat stays at
    // boatPx, but the geographic center drifts). That meant scrolling
    // would flip inPanMode true, lose auto-tracking, and produce jitter
    // as the recenter logic fought the zoom anchor.

    var sz = mapGlobal.map.getSize();

    // Update my boat marker if myBoat is provided
    if (myBoat && mapGlobal.myBoatMarker) {
      var pp = [myBoat.location[1], myBoat.location[0]];
      mapGlobal.myBoatMarker.setGeometry(new Point(pp));

      // Auto-centre on the boat only when it has a usable fix.
      // Otherwise the boat reports [0, 0] (null island) and yanks the
      // view away from the default Hudson-Canyon framing on fresh
      // loads with no GPS yet.
      const boatHasFix = isValidCoordinate(
        myBoat.location[0],
        myBoat.location[1],
      );
      if (!inPanMode && sz && boatHasFix) {
        var boatPx: [number, number] =
          boatPositionMode === "bottom" ? [sz[0] / 2, sz[1] * 0.8] : [sz[0] / 2, sz[1] / 2];
        mapGlobal.view.centerOn(pp, sz, boatPx);

        if (autoZoomActive) {
          // zoom of 10 is about 30 miles, zoom of 16 is city level
          var zoom = Math.pow(Math.floor(myBoat.speed), 0.41);
          zoom = Math.floor(16 - zoom) + (zoomModifier || 0);
          if (zoom <= 0) {
            zoom = 1;
          }
          mapGlobal.view.setZoom(zoom);
          mapInternalState.lastZoom = zoom;
        } else {
          // Auto-zoom off: leave the user's zoom alone, but keep lastZoom
          // in sync so the pan-detection diff check at the top of this
          // function doesn't false-positive a pan from our own re-center.
          var z = mapGlobal.view.getZoom();
          if (typeof z === "number") {
            mapInternalState.lastZoom = z;
          }
        }

        // Record the actual view center, not the boat position — in
        // "bottom" mode centerOn offsets the view so the boat sits at
        // 80% down. Storing pp here would make the next tick's diff
        // check think the user panned and re-enter pan mode.
        const vc = mapGlobal.view.getCenter();
        mapInternalState.lastCenter = vc ? [vc[0], vc[1]] : pp;
      }

      if (pp[0] != 0) {
        recordTrackPoint("myBoat", pp);
      }
    }

    // heading line stuff
    updateHeadingLine();

    // route stuff
    mapGlobal.routeFeatures.clear();
    if (myBoat?.route && myBoat.route.destinationLongitude && myBoat.route.destinationLatitude) {
      var dest = [myBoat.route.destinationLongitude, myBoat.route.destinationLatitude];

      var f = new Feature({
        type: "track",
        geometry: new LineString([mapInternalState.lastPosition, dest]),
      });
      mapGlobal.routeFeatures.push(f);
    }

    // navigation-service route: draws a polyline from the boat through each
    // waypoint in order, plus a circle marker at every waypoint. The line and
    // markers live in separate sources so the Modify interaction can target
    // only the markers (LineStrings would otherwise grow new control points
    // on drag).
    mapGlobal.navRouteLineFeatures.clear();
    if (navWaypoints && navWaypoints.length > 0) {
      const wpCoords: number[][] = navWaypoints.map((wp) => [wp.lng, wp.lat]);

      const startCoord =
        myBoat && isValidCoordinate(myBoat.location[0], myBoat.location[1])
          ? [myBoat.location[1], myBoat.location[0]]
          : null;
      const lineCoords = startCoord ? [startCoord, ...wpCoords] : wpCoords;

      if (lineCoords.length >= 2) {
        // Each segment ends at the waypoint with this ID. When the user picks
        // "add waypoint here" on segment i the new wp is inserted *before*
        // this id, which keeps the segment's right endpoint stable.
        const segmentBeforeIds: string[] = [];
        for (let i = 1; i < lineCoords.length; i++) {
          // line index i corresponds to wp index (i - offset) where offset is
          // 1 if the line starts at the boat, 0 otherwise.
          const wpIdx = startCoord ? i - 1 : i;
          segmentBeforeIds.push(navWaypoints[wpIdx].id);
        }
        mapGlobal.navRouteLineFeatures.push(
          new Feature({
            type: "navRoute",
            segmentBeforeIds,
            geometry: new LineString(lineCoords),
          })
        );
      }
    }
    syncNavWaypointFeatures();

    if (boats == null) {
      mapGlobal.aisFeatures.clear();
    } else {
      var seen: Record<string, boolean> = {};
      boats.forEach((boat) => {
        var mmsi = boat.mmsi;
        if (!mmsi) {
          return;
        }
        seen[mmsi] = true;
        const isVisible = effectiveVisibleBoats.has(mmsi);

        const boatPos = [boat.location[1], boat.location[0]];

        // AIS position history now comes pre-loaded from the viamboat
        // module's `all_history` DoCommand (see App.svelte's AIS poll)
        // and lands in boat.positionHistory below — we no longer
        // accumulate it per-tick in the browser. That accumulation
        // was the main cause of the AIS tab feeling sluggish with
        // many vessels in view.

        for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
          var v = mapGlobal.aisFeatures.item(i) as Feature<Geometry>;
          if (v.get("mmsi") == mmsi) {
            v.setGeometry(new Point(boatPos));
            v.set("speed", boat.speed);
            v.set("heading", boat.heading);
            v.set("cog", boat.cog);
            v.set("name", boat.name);
            v.set("visible", isVisible);
            v.set("length", boat.length);
            v.set("beam", boat.beam);
            return;
          }
        }

        mapGlobal.aisFeatures.push(
          new Feature({
            type: "ais",
            name: boat.name,
            mmsi: mmsi,
            speed: boat.speed,
            heading: boat.heading,
            cog: boat.cog,
            length: boat.length,
            beam: boat.beam,
            visible: isVisible,
            geometry: new Point(boatPos),
          })
        );
      });

      // Iterate backwards so removeAt(i) doesn't shift items we
      // haven't checked yet — a forward loop with a removal in the
      // middle would skip the item that takes the removed slot's
      // place, leaving stale AIS markers behind when several
      // vessels disappear in the same tick.
      for (var i = mapGlobal.aisFeatures.getLength() - 1; i >= 0; i--) {
        var v = mapGlobal.aisFeatures.item(i) as Feature<Geometry>;
        var mmsi = v.get("mmsi") as string;
        if (!seen[mmsi]) {
          mapGlobal.aisFeatures.removeAt(i);
          delete mapInternalState.lastPositions[mmsi];
        }
      }
    }

    // Render historical tracks (clear and re-render when data changes to pick up depth)
    if (positionHistorical) {
      const posKey =
        positionHistorical.length +
        "-" +
        (positionHistorical.length > 0 ? (positionHistorical[0].depth ?? "n") : "");
      if (mapInternalState.lastPosHistoricalKey !== posKey) {
        clearHistoricalTrackFeatures("myBoat");
        mapInternalState.lastPosHistoricalKey = posKey;
      }
      renderHistoricalTrack("myBoat", positionHistorical, "myBoat");
    }

    if (boats) {
      boats.forEach((boat) => {
        if (boat.mmsi && boat.positionHistory && boat.positionHistory.length > 0) {
          renderHistoricalTrack(boat.mmsi, boat.positionHistory, `ais-${boat.mmsi}`);
        }
      });
    }
  }

  // Prune old track features to prevent memory leaks
  function pruneOldTrackFeatures() {
    const maxFeatures = 20000; // Hardcoded limit
    const maxAge = 24 * 60 * 60 * 1000; // 24 hours in milliseconds

    // Remove oldest features if over limit (my boat track)
    if (mapGlobal.trackFeatures.getLength() > maxFeatures) {
      const toRemove = mapGlobal.trackFeatures.getLength() - maxFeatures;
      for (let i = 0; i < toRemove; i++) {
        const trackFeat = mapGlobal.trackFeatures.item(0);
        if (trackFeat) {
          delete mapInternalState.trackFeatureIds[trackFeat.get("myid")];
        }
        mapGlobal.trackFeatures.removeAt(0);
      }
    }

    // Remove oldest features if over limit (AIS track)
    if (mapGlobal.aisTrackFeatures.getLength() > maxFeatures) {
      const toRemove = mapGlobal.aisTrackFeatures.getLength() - maxFeatures;
      for (let i = 0; i < toRemove; i++) {
        const aisFeat = mapGlobal.aisTrackFeatures.item(0);
        if (aisFeat) {
          delete mapInternalState.aisTrackFeatureIds[aisFeat.get("myid")];
        }
        mapGlobal.aisTrackFeatures.removeAt(0);
      }
    }

    // Periodically clear trackFeatureIds to prevent dictionary memory leak
    const now = new Date();
    const timeSinceLastCheck = now.getTime() - mapGlobal.trackFeaturesLastCheck.getTime();
    if (timeSinceLastCheck > maxAge) {
      mapInternalState.trackFeatureIds = {};
      mapInternalState.aisTrackFeatureIds = {};
      mapGlobal.trackFeaturesLastCheck = now;
    }
  }

  // Helper to get track collection info based on boat ID
  function getTrackCollections(boatId: string) {
    const isAis = boatId !== "myBoat";
    return {
      featureIds: isAis ? mapInternalState.aisTrackFeatureIds : mapInternalState.trackFeatureIds,
      features: isAis ? mapGlobal.aisTrackFeatures : mapGlobal.trackFeatures,
      type: isAis ? "ais-track" : "track",
    };
  }

  // S-57 COLOUR codes (csv string of "1".."13") -> CSS hex.
  function s57ColourToCss(code: string): string {
    switch (code.trim()) {
      case "1": return "#ffffff"; // white
      case "2": return "#000000"; // black
      case "3": return "#d9263a"; // red
      case "4": return "#1f9e49"; // green
      case "5": return "#1446cc"; // blue
      case "6": return "#f5d011"; // yellow
      case "7": return "#888888"; // grey
      case "8": return "#8b5a2b"; // brown
      case "9": return "#ffa500"; // amber
      case "10": return "#8246c8"; // violet
      case "11": return "#ff6e00"; // orange
      case "12": return "#c850c8"; // magenta
      case "13": return "#ffb4d2"; // pink
      default: return "#888888";
    }
  }

  function navaidColours(props: any): string[] {
    const raw = props?.COLOUR;
    if (typeof raw !== "string" || !raw) return ["#888888"];
    return raw.split(",").map(s57ColourToCss);
  }

  // S-52 magenta — the colour libS52 / NOAA charts use for light flares.
  const NAVAID_LIGHT_MAGENTA = "#c850c8";

  // Build an SVG marker for a buoy/beacon/light. Cached by composite key
  // (class + shape + colours + lighted) so repeated renders reuse the
  // already-built data URL.
  //
  // SVG canvas is 24×24; the structure's "footprint" sits at (12, 18) —
  // OL Icon anchor below maps that pixel to the chart fix. The upper-right
  // quadrant is reserved for a magenta light flare when the buoy/beacon is
  // co-located with a LIGHTS feature (server-side join).
  const navaidIconCache: Record<string, string> = {};
  function navaidIconSrc(class_: string, props: any): string {
    const colours = navaidColours(props);
    const lighted = props?.lighted === true;
    const shape = Number(
      class_.startsWith("BCN") ? props?.BCNSHP : props?.BOYSHP
    );
    const key = `${class_}|${shape || 0}|${colours.join(",")}|L${lighted ? 1 : 0}`;
    if (navaidIconCache[key]) return navaidIconCache[key];

    const W = 24,
      H = 24;
    const ax = 12, // anchor x in svg pixels
      ay = 18; // anchor y in svg pixels
    const stroke = "#000";
    const sw = 1;

    const isLight = class_ === "LIGHTS";
    const isBeacon = class_.startsWith("BCN");
    const isDay = class_ === "DAYMAR";

    let body = "";
    if (isLight) {
      // Standalone light (lighthouse / sector light): magenta starburst.
      const c = colours[0] === "#888888" ? NAVAID_LIGHT_MAGENTA : colours[0];
      body =
        `<polygon points="${ax},${ay - 8} ${ax + 1.2},${ay - 1.5} ${ax + 7},${ay} ${ax + 1.2},${ay + 1.5} ${ax},${ay + 7} ${ax - 1.2},${ay + 1.5} ${ax - 7},${ay} ${ax - 1.2},${ay - 1.5}" ` +
        `fill="${c}" stroke="${stroke}" stroke-width="0.6"/>`;
    } else if (isBeacon) {
      body = beaconBody(colours, ax, ay, stroke, sw);
    } else if (isDay) {
      // Daymark: small filled diamond.
      const c = colours[0];
      body =
        `<polygon points="${ax},${ay - 7} ${ax + 5},${ay - 2} ${ax},${ay + 3} ${ax - 5},${ay - 2}" ` +
        `fill="${c}" stroke="${stroke}" stroke-width="${sw}"/>`;
    } else {
      // Buoy — silhouette per BOYSHP enum.
      body = buoyBody(shape, colours, ax, ay, stroke, sw);
    }

    // Lighted overlay: magenta wedge "flag" extending up-and-right from
    // the structure. S-52 draws a filled triangle in the same hue —
    // unmistakable on a chart even at small symbol size.
    let flare = "";
    if (lighted && !isLight) {
      flare =
        `<polygon points="${ax - 1},${ay - 9} ${ax + 9},${ay - 12} ${ax + 1},${ay - 6}" ` +
        `fill="${NAVAID_LIGHT_MAGENTA}" stroke="${stroke}" stroke-width="0.5"/>`;
    }

    const svg =
      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${H}" width="${W}" height="${H}">` +
      body +
      flare +
      `</svg>`;
    const src = "data:image/svg+xml;utf8," + encodeURIComponent(svg);
    navaidIconCache[key] = src;
    return src;
  }

  // Buoy silhouette per S-57 BOYSHP enum, anchored bottom-centre at (ax, ay).
  // Two-colour bands: vertical halves for round shapes, horizontal bands for
  // tall shapes — matches how banded buoys read off a chart.
  function buoyBody(
    shape: number,
    colours: string[],
    ax: number,
    ay: number,
    stroke: string,
    sw: number
  ): string {
    const c1 = colours[0];
    const c2 = colours[1] ?? colours[0];
    switch (shape) {
      case 1: {
        // Conical, point up.
        const h = 11,
          w = 8;
        return (
          `<defs><clipPath id="b"><polygon points="${ax},${ay - h} ${ax + w / 2},${ay} ${ax - w / 2},${ay}"/></clipPath></defs>` +
          `<g clip-path="url(#b)">` +
          `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h / 2}" fill="${c1}"/>` +
          `<rect x="${ax - w / 2}" y="${ay - h / 2}" width="${w}" height="${h / 2}" fill="${c2}"/>` +
          `</g>` +
          `<polygon points="${ax},${ay - h} ${ax + w / 2},${ay} ${ax - w / 2},${ay}" fill="none" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
      case 2: {
        // Can / cylindrical.
        const h = 9,
          w = 8;
        return (
          `<defs><clipPath id="b"><rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}"/></clipPath></defs>` +
          `<g clip-path="url(#b)">` +
          `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h / 2}" fill="${c1}"/>` +
          `<rect x="${ax - w / 2}" y="${ay - h / 2}" width="${w}" height="${h / 2}" fill="${c2}"/>` +
          `</g>` +
          `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}" fill="none" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
      case 3: {
        // Spherical.
        const r = 5;
        return (
          `<defs><clipPath id="b"><circle cx="${ax}" cy="${ay - r}" r="${r}"/></clipPath></defs>` +
          `<g clip-path="url(#b)">` +
          `<rect x="${ax - r}" y="${ay - 2 * r}" width="${r}" height="${2 * r}" fill="${c1}"/>` +
          `<rect x="${ax}" y="${ay - 2 * r}" width="${r}" height="${2 * r}" fill="${c2}"/>` +
          `</g>` +
          `<circle cx="${ax}" cy="${ay - r}" r="${r}" fill="none" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
      case 4: {
        // Pillar.
        const h = 13,
          w = 6;
        return (
          `<defs><clipPath id="b"><rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}"/></clipPath></defs>` +
          `<g clip-path="url(#b)">` +
          `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h / 2}" fill="${c1}"/>` +
          `<rect x="${ax - w / 2}" y="${ay - h / 2}" width="${w}" height="${h / 2}" fill="${c2}"/>` +
          `</g>` +
          `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}" fill="none" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
      case 5: {
        // Spar.
        const h = 14,
          w = 3;
        return (
          `<defs><clipPath id="b"><rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}"/></clipPath></defs>` +
          `<g clip-path="url(#b)">` +
          `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h / 2}" fill="${c1}"/>` +
          `<rect x="${ax - w / 2}" y="${ay - h / 2}" width="${w}" height="${h / 2}" fill="${c2}"/>` +
          `</g>` +
          `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}" fill="none" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
      case 6: {
        // Barrel.
        const ry = 4,
          rx = 6;
        return (
          `<ellipse cx="${ax}" cy="${ay - ry}" rx="${rx}" ry="${ry}" fill="${c1}" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
      case 7: {
        // Super-buoy.
        const r = 7;
        return (
          `<defs><clipPath id="b"><circle cx="${ax}" cy="${ay - r}" r="${r}"/></clipPath></defs>` +
          `<g clip-path="url(#b)">` +
          `<rect x="${ax - r}" y="${ay - 2 * r}" width="${r}" height="${2 * r}" fill="${c1}"/>` +
          `<rect x="${ax}" y="${ay - 2 * r}" width="${r}" height="${2 * r}" fill="${c2}"/>` +
          `</g>` +
          `<circle cx="${ax}" cy="${ay - r}" r="${r}" fill="none" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
      default: {
        // Unknown / unspecified shape — small filled circle in primary
        // colour. Most cells set BOYSHP; falling back to a generic dot
        // keeps the chart usable when they don't.
        return (
          `<circle cx="${ax}" cy="${ay - 4}" r="4" fill="${c1}" stroke="${stroke}" stroke-width="${sw}"/>`
        );
      }
    }
  }

  // Beacons render as an upright bar with a chart-black footprint dot —
  // visually distinct from buoys (which sit on the water surface). BCNSHP
  // distinctions live in the topmark; we don't draw topmarks at this size.
  function beaconBody(
    colours: string[],
    ax: number,
    ay: number,
    stroke: string,
    sw: number
  ): string {
    const c1 = colours[0];
    const c2 = colours[1] ?? colours[0];
    const h = 13,
      w = 4;
    return (
      `<defs><clipPath id="bc"><rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}"/></clipPath></defs>` +
      `<g clip-path="url(#bc)">` +
      `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h / 2}" fill="${c1}"/>` +
      `<rect x="${ax - w / 2}" y="${ay - h / 2}" width="${w}" height="${h / 2}" fill="${c2}"/>` +
      `</g>` +
      `<rect x="${ax - w / 2}" y="${ay - h}" width="${w}" height="${h}" fill="none" stroke="${stroke}" stroke-width="${sw}"/>` +
      `<circle cx="${ax}" cy="${ay}" r="1" fill="${stroke}"/>`
    );
  }

  function navaidStyleFunction(feature: Feature<Geometry>): Style {
    const props = feature.getProperties();
    const class_ = (props.class as string) ?? "";
    return new Style({
      image: new Icon({
        src: navaidIconSrc(class_, props),
        // Anchor at the structure's footprint inside the SVG: x=12/24,
        // y=18/24 → puts the chart fix at the buoy's bottom and lets the
        // light flare float above.
        anchor: [0.5, 0.75],
      }),
    });
  }


  // Human-readable label for an S-57 class code. Used in hover tooltips.
  function navaidClassLabel(c: string): string {
    switch (c) {
      case "BOYLAT": return "Lateral buoy";
      case "BOYCAR": return "Cardinal buoy";
      case "BOYISD": return "Isolated-danger buoy";
      case "BOYSAW": return "Safe-water buoy";
      case "BOYSPP": return "Special-purpose buoy";
      case "BOYINB": return "Installation buoy";
      case "BCNLAT": return "Lateral beacon";
      case "BCNCAR": return "Cardinal beacon";
      case "BCNISD": return "Isolated-danger beacon";
      case "BCNSAW": return "Safe-water beacon";
      case "BCNSPP": return "Special-purpose beacon";
      case "LIGHTS": return "Light";
      case "DAYMAR": return "Daymark";
      default:       return c;
    }
  }

  // S-57 LITCHR enum -> short S-52 code (F, Fl, Q, Iso, Oc, …).
  function lightCharLabel(code: number): string {
    switch (code) {
      case 1: return "F";
      case 2: return "Fl";
      case 3: return "Fl";
      case 4: return "Q";
      case 5: return "VQ";
      case 6: return "UQ";
      case 7: return "Iso";
      case 8: return "Iso";
      case 9: return "Oc";
      case 10: return "Oc";
      case 11: return "Mo";
      case 12: return "FFl";
      case 13: return "FFl";
      default: return "";
    }
  }

  // S-57 COLOUR csv -> single-letter code list (W/R/G/Y/etc).
  function colourLetters(csv: string): string {
    const map: Record<string, string> = {
      "1": "W", "2": "Bk", "3": "R", "4": "G", "5": "Bu",
      "6": "Y", "7": "Gy", "8": "Br", "9": "Am", "10": "Vi",
      "11": "Or", "12": "Mg", "13": "Pk",
    };
    return csv
      .split(",")
      .map((c) => map[c.trim()] ?? "")
      .filter(Boolean)
      .join("");
  }

  function escapeHtml(s: string): string {
    return s
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  function formatNavaidTooltip(props: any): string {
    const class_ = String(props.class ?? "");
    const lines: string[] = [];
    const title = props.OBJNAM
      ? escapeHtml(String(props.OBJNAM))
      : navaidClassLabel(class_);
    lines.push(`<div class="navaid-tt-title">${title}</div>`);
    lines.push(
      `<div class="navaid-tt-sub">${escapeHtml(navaidClassLabel(class_))}</div>`
    );

    // Light characteristic line: e.g. "Fl R 5s 65ft 12nm". For a buoy
    // that's been server-side joined to a co-located LIGHTS feature the
    // attributes live under LIGHT_*; standalone LIGHTS features carry the
    // attributes directly.
    const litchr = props.LITCHR ?? props.LIGHT_LITCHR;
    const isLighted = class_ === "LIGHTS" || props.lighted === true;
    if (isLighted) {
      const parts: string[] = [];
      const litchrVal = litchr != null ? Number(litchr) : null;
      if (litchrVal !== null) {
        const lc = lightCharLabel(litchrVal);
        if (lc) parts.push(lc);
      }
      const siggrp = props.SIGGRP ?? props.LIGHT_SIGGRP;
      if (siggrp) parts.push(escapeHtml(String(siggrp)));
      const litColour = props.LIGHT_COLOUR ?? props.COLOUR;
      if (typeof litColour === "string") {
        const letters = colourLetters(litColour);
        if (letters) parts.push(letters);
      }
      const sigper = props.SIGPER ?? props.LIGHT_SIGPER;
      if (sigper != null) parts.push(`${sigper}s`);
      const height = props.HEIGHT ?? props.LIGHT_HEIGHT;
      if (height != null) {
        const ft = Math.round(Number(height) * 3.28084);
        parts.push(`${ft}ft`);
      }
      const valnmr = props.VALNMR ?? props.LIGHT_VALNMR;
      if (valnmr != null) parts.push(`${valnmr}nm`);
      if (parts.length) {
        lines.push(`<div class="navaid-tt-row">${parts.join(" ")}</div>`);
      }
    }

    // Sector range, when reported.
    const sectr1 = props.SECTR1 ?? props.LIGHT_SECTR1;
    const sectr2 = props.SECTR2 ?? props.LIGHT_SECTR2;
    if (sectr1 != null && sectr2 != null) {
      lines.push(
        `<div class="navaid-tt-row">Sector ${sectr1}°–${sectr2}°</div>`
      );
    }

    if (props.INFORM) {
      lines.push(
        `<div class="navaid-tt-info">${escapeHtml(String(props.INFORM))}</div>`
      );
    }

    return lines.join("");
  }

  // S-57 CATBRG enum → human-readable bridge category.
  function bridgeCategoryLabel(code: number): string {
    switch (code) {
      case 1: return "Fixed";
      case 2: return "Opening";
      case 3: return "Swing";
      case 4: return "Lifting";
      case 5: return "Bascule";
      case 6: return "Pontoon";
      case 7: return "Drawbridge";
      case 8: return "Transporter";
      case 9: return "Footbridge";
      case 10: return "Viaduct";
      case 11: return "Aqueduct";
      case 12: return "Suspension";
      default: return "";
    }
  }

  function structureClassLabel(c: string): string {
    switch (c) {
      case "BRIDGE": return "Bridge";
      case "CBLOHD": return "Overhead cable";
      case "PIPOHD": return "Overhead pipe";
      case "CONVYR": return "Conveyor";
      default:       return c;
    }
  }

  // Build the SVG icon for a structure feature (bridges / overhead
  // cables / overhead pipes / conveyors). Compact 24x24 box with a
  // class-distinguishing glyph: a stylised bridge arch for BRIDGE,
  // overhead horizontal line + vertical drop for cables/pipes/conveyors.
  function structureIconSrc(class_: string): string {
    const stroke = class_ === "BRIDGE" ? "#854d0e" : "#b45309";
    const fill = class_ === "BRIDGE" ? "#facc15" : "#fde68a";
    let glyph: string;
    if (class_ === "BRIDGE") {
      // Two arches with a horizontal deck on top.
      glyph =
        `<path d="M2 18 H22" stroke="${stroke}" stroke-width="2.5" stroke-linecap="round" fill="none"/>` +
        `<path d="M4 18 Q4 11 10 11 Q16 11 16 18" stroke="${stroke}" stroke-width="1.5" fill="${fill}"/>` +
        `<path d="M12 18 Q12 13 16 13 Q20 13 20 18" stroke="${stroke}" stroke-width="1.5" fill="${fill}"/>`;
    } else {
      // Overhead utility: horizontal sky line with a vertical drop and a
      // small "↕" hint inside.
      const accent = class_ === "PIPOHD" ? "#7c2d12" : stroke;
      glyph =
        `<path d="M2 7 H22" stroke="${accent}" stroke-width="2" stroke-linecap="round" fill="none"/>` +
        `<path d="M6 7 V19 M18 7 V19" stroke="${accent}" stroke-width="1.5" stroke-linecap="round" fill="none"/>` +
        `<path d="M12 9 V19 M9 12 L12 9 L15 12 M9 16 L12 19 L15 16" stroke="${accent}" stroke-width="1.2" fill="none"/>`;
    }
    const svg =
      `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24">` +
      `<circle cx="12" cy="12" r="11" fill="rgba(255,255,255,0.85)" stroke="${stroke}" stroke-width="1"/>` +
      glyph +
      `</svg>`;
    return "data:image/svg+xml;base64," + btoa(svg);
  }

  function structureStyleFunction(feature: Feature<Geometry>): Style[] {
    const props = feature.getProperties();
    const class_ = (props.class as string) ?? "";
    const geom = feature.getGeometry();
    const geomType = geom?.getType();
    const styles: Style[] = [];
    if (
      geomType === "LineString" ||
      geomType === "Polygon" ||
      geomType === "MultiLineString" ||
      geomType === "MultiPolygon"
    ) {
      // At z>=14 the tile is built with skip=BRIDGE,CBLOHD,... so this
      // vector trace is the *sole* renderer for the structure — it has to
      // stand in for the bold chart symbol, not just hint at it. So draw
      // bridges as a solid amber bar with a thick dark outline, and
      // overhead cables/pipes/conveyors as a thick dashed line (the dash
      // reads as "overhead"). The icon (added below) still pins the hover
      // anchor. A translucent fill here would wash out and read as "no
      // bridge, just the label".
      const isBridge = class_ === "BRIDGE";
      styles.push(
        new Style({
          stroke: new Stroke({
            color: isBridge ? "rgba(120,53,15,0.95)" : "rgba(154,52,18,0.95)",
            width: isBridge ? 3 : 3,
            // Solid for bridges (a physical deck); dashed for overhead
            // cables/pipes/conveyors so they don't read as a solid span.
            lineDash: isBridge ? undefined : [8, 5],
          }),
          fill: new Fill({
            color: isBridge
              ? "rgba(250,204,21,0.55)"
              : "rgba(253,230,138,0.4)",
          }),
        })
      );
    }
    // hideIcon: backend has flagged this feature as a duplicate of (or
    // info-equivalent to) another same-named structure, so the trace
    // above still draws but the icon belongs to the canonical entry.
    if (props.hideIcon !== true) {
      styles.push(
        new Style({
          image: new Icon({
            src: structureIconSrc(class_),
            anchor: [0.5, 0.5],
          }),
          // For line/polygon features, render the icon at the first
          // vertex so the hover target is predictable. For point
          // features, OL uses the point itself.
          geometry:
            geomType === "LineString" && geom
              ? new Point((geom as any).getFirstCoordinate())
              : geomType === "Polygon" && geom
                ? new Point((geom as any).getInteriorPoint().getCoordinates())
                : undefined,
        })
      );
    }
    return styles;
  }

  function formatStructureTooltip(props: any): string {
    const class_ = String(props.class ?? "");
    const lines: string[] = [];
    const title = props.OBJNAM
      ? escapeHtml(String(props.OBJNAM))
      : structureClassLabel(class_);
    lines.push(`<div class="navaid-tt-title">${title}</div>`);
    lines.push(
      `<div class="navaid-tt-sub">${escapeHtml(structureClassLabel(class_))}</div>`
    );
    const meta: string[] = [];
    if (class_ === "BRIDGE" && props.CATBRG != null) {
      const label = bridgeCategoryLabel(Number(props.CATBRG));
      if (label) meta.push(label);
    }
    // Vertical clearance — VERCLR is the canonical value; specific
    // variants (closed, open, safe) get their own lines when present.
    const fmtClr = (v: any) => `${Number(v).toFixed(1)} m`;
    if (props.VERCLR != null) meta.push(`Vert clr ${fmtClr(props.VERCLR)}`);
    if (props.VERCCL != null) meta.push(`Closed ${fmtClr(props.VERCCL)}`);
    if (props.VERCOP != null) meta.push(`Open ${fmtClr(props.VERCOP)}`);
    if (props.VERCSA != null) meta.push(`Safe ${fmtClr(props.VERCSA)}`);
    if (props.HORCLR != null) meta.push(`Horz clr ${fmtClr(props.HORCLR)}`);
    if (meta.length > 0) {
      lines.push(`<div class="navaid-tt-sub">${escapeHtml(meta.join(" · "))}</div>`);
    }
    if (props.INFORM) {
      lines.push(`<div class="navaid-tt-sub">${escapeHtml(String(props.INFORM))}</div>`);
    } else if (props.NINFOM) {
      lines.push(`<div class="navaid-tt-sub">${escapeHtml(String(props.NINFOM))}</div>`);
    }
    return lines.join("");
  }

  function createDetectionStyle(): Style {
    return new Style({
      image: new RegularShape({
        fill: new Fill({ color: "rgba(0, 220, 140, 0.35)" }),
        stroke: new Stroke({ color: "white", width: 2 }),
        points: 3,
        radius: 10,
        rotation: 0,
        angle: 0,
      }),
    });
  }

  // Cache SVG data URLs keyed by length-scale bucket — Icon construction is
  // cheap but URL-encoding the SVG every render isn't free.
  const aisTriangleSrcCache: Record<string, string> = {};
  function aisTriangleSrc(lengthScale: number): string {
    const key = lengthScale.toFixed(2);
    if (aisTriangleSrcCache[key]) return aisTriangleSrcCache[key];
    const baseW = 12;
    const baseH = 24; // always 2x the base width
    const sw = 2;
    const w = baseW;
    const h = baseH * lengthScale;
    const inset = sw / 2;
    const points =
      `${w / 2},${inset} ` +
      `${w - inset},${h - inset} ` +
      `${inset},${h - inset}`;
    const svg =
      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${w} ${h}" ` +
      `width="${w}" height="${h}">` +
      `<polygon points="${points}" fill="none" stroke="#1e6fff" ` +
      `stroke-width="${sw}" stroke-linejoin="round"/></svg>`;
    const src = "data:image/svg+xml;utf8," + encodeURIComponent(svg);
    aisTriangleSrcCache[key] = src;
    return src;
  }

  function aisStyleFunction(feature: Feature<Geometry>): Style[] {
    const visible = feature.get("visible") ?? false;
    if (!visible) return [];

    const cog = feature.get("cog") as number | null | undefined;
    const heading = feature.get("heading") as number | null | undefined;
    const speed = feature.get("speed") as number | null | undefined;
    const length = feature.get("length") as number | null | undefined;
    // Heading of exactly 0 from AIS usually means "unknown" rather than
    // genuinely pointing north — fall back to COG when available.
    const direction =
      heading != null && heading !== 0 ? heading : (cog ?? 0);
    const rotation = (direction * Math.PI) / 180;

    // Triangle is always 2:1 (height:width) at the default vessel length;
    // longer boats stretch the height further. Capped by myBoat's length
    // so no AIS target renders longer than the user's own boat.
    const lengthScale = dimScaleFactor(length, DEFAULT_BOAT_LENGTH_M);
    const myLengthScale = dimScaleFactor(myBoat?.length, DEFAULT_BOAT_LENGTH_M);
    const cappedScale = Math.min(lengthScale, myLengthScale);

    const styles: Style[] = [
      new Style({
        image: new Icon({
          src: aisTriangleSrc(cappedScale),
          rotation: rotation,
          rotateWithView: true,
        }),
      }),
    ];

    if (cog != null && Number.isFinite(cog) && speed != null && speed > 1) {
      const projOption = mapGlobal.layerOptions.find((l) => l.name === "ais-projection");
      if (projOption?.on) {
        const geom = feature.getGeometry();
        if (geom && geom.getType() === "Point") {
          const start = (geom as Point).getCoordinates();
          const distMeters = speed * 1852 * (aisProjectionMinutes / 60);
          const bearingRad = (cog * Math.PI) / 180;
          const tip = sphereOffset(start, distMeters, bearingRad);
          styles.push(
            new Style({
              geometry: new LineString([start, tip]),
              stroke: new Stroke({
                color: "#1e6fff",
                width: 2,
                lineDash: [4, 4],
              }),
            })
          );
        }
      }
    }

    return styles;
  }

  function getPositionAtTime(
    history: PositionPoint[],
    targetTime: Date
  ): { lat: number; lng: number } | null {
    if (history.length === 0) return null;

    const targetMs = targetTime.getTime();
    const closest = history.reduce((a, b) =>
      Math.abs(a.ts.getTime() - targetMs) <= Math.abs(b.ts.getTime() - targetMs) ? a : b
    );

    return { lat: closest.lat, lng: closest.lng };
  }

  function renderDetections(detections: Detection[] | undefined) {
    // Remove old detection features from aisTrackFeatures
    const toRemove: Feature<Geometry>[] = [];
    mapGlobal.aisTrackFeatures.forEach((f) => {
      if (f.get("type") === "detection") toRemove.push(f);
    });
    toRemove.forEach((f) => mapGlobal.aisTrackFeatures.remove(f));

    if (!detections || detections.length === 0) return;

    const allHistories: Record<string, PositionPoint[]> = {};

    if (positionHistorical && positionHistorical.length > 0) {
      allHistories["myBoat"] = positionHistorical;
    }

    boats?.forEach((boat) => {
      if (boat.positionHistory && boat.positionHistory.length > 0) {
        const key = boat.mmsi || boat.partId || "unknown";
        allHistories[key] = boat.positionHistory;
      }
    });

    detections.forEach((detection) => {
      const history = allHistories[detection.boatId];
      if (!history) return;

      const position = getPositionAtTime(history, detection.timestamp);
      if (!position) return;

      const feature = new Feature({
        type: "detection",
        boatId: detection.boatId,
        detectionId: detection.id,
        timestamp: detection.timestamp,
        detectionData: detection,
        geometry: new Point([position.lng, position.lat]),
      });

      mapGlobal.aisTrackFeatures.push(feature);
    });
  }

  // Factory to create track style functions (DRY for myBoat and AIS tracks)
  function depthToColor(depth: number, opacity: number): string {
    // 0ft = red, 10ft+ = green, linear scale
    const t = Math.min(Math.max(depth / 10, 0), 1);
    const r = Math.round(255 * (1 - t));
    const g = Math.round(255 * t);
    return `rgba(${r}, ${g}, 0, ${opacity})`;
  }

  function createTrackStyleFunction(defaultBoatId: string) {
    return function (feature: any) {
      const boatId = feature.get("boatId") || defaultBoatId;
      if (!effectiveVisibleBoats.has(boatId)) {
        return new Style({}); // Hidden - return empty style
      }

      // When a popup is open on a boat, suppress every other boat's track
      // (and its detections) so only the selected target's history shows.
      if (popupState.visible) {
        const selectedBoatId = popupState.content.isMyBoat
          ? "myBoat"
          : popupState.content.mmsi;
        if (selectedBoatId && boatId !== selectedBoatId) {
          return new Style({});
        }
      }

      // Detection features get triangle style
      if (feature.get("type") === "detection") {
        return createDetectionStyle();
      }

      const isGap = feature.get("isGap");
      const opacity = isGap ? 0.33 : 1.0;
      const depth = feature.get("depth");

      let color;
      if (depthColorTrack && depth !== undefined && depth !== null) {
        color = depthToColor(depth, opacity);
      } else {
        color = `rgba(0, 0, 255, ${opacity})`;
      }

      return new Style({
        stroke: new Stroke({
          color: color,
          width: 2,
          lineDash: isGap ? [2, 6] : undefined,
        }),
      });
    };
  }

  function clearHistoricalTrackFeatures(boatId: string): void {
    const { featureIds, features } = getTrackCollections(boatId);
    // Remove features that have a "myid" (historical) — keep live track features
    const toRemove: Feature<Geometry>[] = [];
    for (let i = 0; i < features.getLength(); i++) {
      const f = features.item(i);
      const myid = f.get("myid");
      if (myid) {
        toRemove.push(f);
        delete featureIds[myid];
      }
    }
    toRemove.forEach((f) => features.remove(f));
  }

  function addTrackFeature(
    id: string,
    g: Geometry,
    boatId: string = "myBoat",
    isGap: boolean = false,
    depth?: number,
    ts?: number
  ) {
    const { featureIds, features, type } = getTrackCollections(boatId);

    if (featureIds[id] == true) {
      return;
    }

    featureIds[id] = true;

    features.push(
      new Feature({
        type: type,
        boatId: boatId,
        myid: id,
        geometry: g,
        isGap: isGap,
        depth: depth,
        // Millis. Records when the boat arrived at the *end* of this
        // segment so the hover tooltip can answer "what time was I here?"
        ts: ts,
      })
    );

    pruneOldTrackFeatures();
  }

  // Record a track point for any boat, updating lastPositions and adding feature if moved
  function recordTrackPoint(boatId: string, position: number[]): void {
    const lastPosKey = boatId === "myBoat" ? null : boatId;
    const lastPos = lastPosKey
      ? mapInternalState.lastPositions[lastPosKey]
      : mapInternalState.lastPosition;

    if (!lastPos || lastPos[0] === 0) {
      if (lastPosKey) {
        mapInternalState.lastPositions[lastPosKey] = position;
      } else {
        mapInternalState.lastPosition = position;
      }
      return;
    }

    const diff = pointDiff(lastPos, position);
    if (diff > 0.0000001) {
      const { features, type } = getTrackCollections(boatId);

      features.push(
        new Feature({
          type: type,
          boatId: boatId,
          geometry: new LineString([lastPos, position]),
          // Time the boat arrived at `position`. Powers the hover-time
          // tooltip; the realtime tail array tracked just below is for
          // a different purpose (deduping vs historical render).
          ts: Date.now(),
        })
      );

      // Record the timestamp so renderHistoricalTrack can tell which
      // historical points are already covered by realtime. Append-only
      // sorted (Date.now() is monotonic-ish for our purposes); prune the
      // tail to keep the array bounded.
      const now = Date.now();
      let arr = mapInternalState.realtimeTrackTs[boatId];
      if (!arr) {
        arr = [];
        mapInternalState.realtimeTrackTs[boatId] = arr;
      }
      arr.push(now);
      const dropBefore = now - realtimeTsKeepMs;
      let dropCount = 0;
      while (dropCount < arr.length && arr[dropCount] < dropBefore) {
        dropCount++;
      }
      if (dropCount > 0) {
        arr.splice(0, dropCount);
      }

      if (lastPosKey) {
        mapInternalState.lastPositions[lastPosKey] = position;
      } else {
        mapInternalState.lastPosition = position;
      }

      pruneOldTrackFeatures();
    }
  }

  // Calculate distance between two points in nautical miles
  // Using Haversine formula for great circle distance
  function calculateDistanceNM(lat1: number, lng1: number, lat2: number, lng2: number): number {
    const R = 3440.065; // Earth's radius in nautical miles
    const dLat = ((lat2 - lat1) * Math.PI) / 180;
    const dLng = ((lng2 - lng1) * Math.PI) / 180;
    const a =
      Math.sin(dLat / 2) * Math.sin(dLat / 2) +
      Math.cos((lat1 * Math.PI) / 180) *
        Math.cos((lat2 * Math.PI) / 180) *
        Math.sin(dLng / 2) *
        Math.sin(dLng / 2);
    const c = 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
    return R * c;
  }

  // realtimeCoversTs returns true if realtime has a recorded point within
  // realtimeMatchToleranceMs of `ts`. Implemented as a binary search on
  // the sorted-append-only timestamp list per boat.
  function realtimeCoversTs(boatId: string, ts: number): boolean {
    const arr = mapInternalState.realtimeTrackTs[boatId];
    if (!arr || arr.length === 0) return false;
    let lo = 0;
    let hi = arr.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (arr[mid] < ts) lo = mid + 1;
      else hi = mid;
    }
    // Closest entry is at lo or lo-1; check both.
    const a = lo > 0 ? Math.abs(arr[lo - 1] - ts) : Infinity;
    const b = lo < arr.length ? Math.abs(arr[lo] - ts) : Infinity;
    return Math.min(a, b) <= realtimeMatchToleranceMs;
  }

  // Render historical track from position history array.
  // Draws dotted 33% transparent lines between points that are 10+ nautical miles apart.
  //
  // Hand-off rule with the realtime track: realtime is the source of truth
  // for the last realtimeWindowMs (10 min). Historical only paints inside
  // that window where realtime has a gap (no recorded point within
  // realtimeMatchToleranceMs of the historical point's timestamp). Outside
  // the window historical paints unconditionally. Net effect: no
  // double-painting of the recent past, but disconnections / sensor blips
  // in realtime get filled in by historical.
  function renderHistoricalTrack(
    boatId: string,
    history: PositionPoint[],
    idPrefix: string
  ): void {
    const now = Date.now();
    const realtimeWindowStart = now - realtimeWindowMs;

    let prev: number[] | null = null;
    let prevPoint: PositionPoint | null = null;

    history.forEach((p) => {
      const pp = [p.lng, p.lat];

      if (prev && prevPoint) {
        // Some history sources (e.g. legacy AIS positionHistory, or albertboat
        // fetch) don't carry per-point timestamps. Without a ts we can't run
        // the realtime hand-off check, so paint unconditionally — same
        // behaviour as before the hand-off rule existed.
        let ts: number | null = null;
        if (p.ts instanceof Date) {
          const t = p.ts.getTime();
          if (!Number.isNaN(t)) ts = t;
        }
        let skip = false;
        if (ts !== null && ts >= realtimeWindowStart && realtimeCoversTs(boatId, ts)) {
          skip = true;
        }

        if (!skip) {
          // Calculate distance between consecutive points
          const distanceNM = calculateDistanceNM(prevPoint.lat, prevPoint.lng, p.lat, p.lng);

          // Mark as gap if points are more than 10 nautical miles apart
          const isGap = distanceNM >= 10;

          addTrackFeature(
            `${idPrefix}-line-${p.lng}-${p.lat}`,
            new LineString([prev, pp]),
            boatId,
            isGap,
            p.depth,
            ts ?? undefined
          );
        }
      }

      prev = pp;
      prevPoint = p;
    });
  }

  async function probeMyBoatIcon() {
    try {
      const resp = await fetch("/myboat-icon", { method: "HEAD" });
      if (!resp.ok) return;
      // Cache-bust against tileGenVersion so a new build picks up an icon
      // swap on the server even if the browser cached the old bytes.
      const url = "/myboat-icon?v=" + tileGenVersion;
      // Preload to read natural dimensions — we need them to remap scale.
      const img = new Image();
      const loaded = new Promise<void>((resolve, reject) => {
        img.onload = () => resolve();
        img.onerror = () => reject(new Error("decode failed"));
      });
      img.src = url;
      await loaded;
      myBoatImageNaturalHeight = img.naturalHeight || null;
      myBoatImageNaturalWidth = img.naturalWidth || null;
      myBoatImage = url;
      // Force the layer to re-evaluate its style with the new src/scale.
      mapGlobal.myBoatMarker?.changed();
    } catch {
      // Endpoint not present, fetch failed, or image failed to decode —
      // keep the bundled default.
    }
  }

  // Create boat icon style. `src` defaults to the bundled boat image; pass
  // `myBoatImage` for the user's own boat so a configured override doesn't
  // bleed into AIS markers. When `src` is the override, the scale is
  // remapped by the height ratio to keep the rendered size matched to the
  // bundled icon regardless of the override's source resolution.
  function createBoatStyle(
    heading: number,
    scale: number | [number, number],
    visible: boolean,
    src: string = boatImage
  ): Style {
    if (!visible) {
      return new Style({}); // Empty style = hidden
    }

    const rotation = (heading / 360) * Math.PI * 2;
    let sx = typeof scale === "number" ? scale : scale[0];
    let sy = typeof scale === "number" ? scale : scale[1];

    // The override-resize branch only applies to the user-configured myBoat
    // icon (when an override has actually loaded), not to the bundled AIS
    // variant which already matches the default natural dimensions.
    const isOverride =
      src === myBoatImage && myBoatImage !== boatImage;
    if (isOverride && myBoatImageNaturalHeight && myBoatImageNaturalHeight > 0) {
      const ratio = BOAT_IMAGE_NATURAL_HEIGHT / myBoatImageNaturalHeight;
      sx *= ratio;
      sy *= ratio;
      // Enforce a minimum rendered width so a narrow override doesn't end
      // up as a sliver on the chart. Bumps both axes uniformly — aspect
      // ratio is preserved, the icon just grows past the height-matched
      // size when its width would otherwise be below the floor.
      if (myBoatImageNaturalWidth && myBoatImageNaturalWidth > 0) {
        const renderedWidth = myBoatImageNaturalWidth * sx;
        if (renderedWidth < MYBOAT_MIN_RENDERED_WIDTH_PX) {
          const bump = MYBOAT_MIN_RENDERED_WIDTH_PX / renderedWidth;
          sx *= bump;
          sy *= bump;
        }
      }
    }

    return new Style({
      image: new Icon({
        src: src,
        scale: [sx, sy],
        rotation: rotation,
        rotateWithView: true,
      }),
    });
  }

  function toggleMeasure() {
    if (measureActive) {
      stopMeasure();
    } else {
      measureActive = true;
      measurePoints = [];
      measureDistance = null;
      if (measureSource) measureSource.clear();
      addWaypointActive = false;
      closePopup();
    }
  }

  function stopMeasure() {
    measureActive = false;
    measurePoints = [];
    measureDistance = null;
    if (measureSource) measureSource.clear();
  }

  function toggleAddWaypoint() {
    if (!onAddWaypoint) return;
    addWaypointActive = !addWaypointActive;
    if (addWaypointActive) {
      // Mutually exclusive with measure mode.
      if (measureActive) stopMeasure();
      closePopup();
    }
  }

  // Two-step confirmation for the clear-all-waypoints button. First click
  // arms the confirm state; second click within CLEAR_CONFIRM_MS commits.
  // Auto-disarms after the timeout so a forgotten click doesn't lurk.
  const CLEAR_CONFIRM_MS = 4000;
  let clearConfirmArmed = $state(false);
  let clearConfirmTimer: number | undefined;

  function clearWaypoints() {
    if (!clearConfirmArmed) {
      clearConfirmArmed = true;
      if (clearConfirmTimer !== undefined) clearTimeout(clearConfirmTimer);
      clearConfirmTimer = setTimeout(() => {
        clearConfirmArmed = false;
        clearConfirmTimer = undefined;
      }, CLEAR_CONFIRM_MS) as unknown as number;
      return;
    }
    if (clearConfirmTimer !== undefined) {
      clearTimeout(clearConfirmTimer);
      clearConfirmTimer = undefined;
    }
    clearConfirmArmed = false;
    addWaypointActive = false;
    onClearWaypoints?.();
  }

  // Reconcile the waypoint marker collection against the latest navWaypoints
  // prop. We mutate-in-place rather than clearing/repopulating because the
  // Modify interaction holds direct references to the Feature objects: blowing
  // them away mid-drag would drop the user's gesture. While a marker is being
  // dragged we leave it untouched so the drag can finish cleanly.
  function syncNavWaypointFeatures() {
    const desired = navWaypoints ?? [];
    // Plain object lookup by id; the OL `Map` import shadows the global
    // Map constructor in this module, so `new Map(...)` would build a map
    // widget instead of a hash and blow up on `.has(...)`.
    const desiredById: Record<string, true> = {};
    for (const wp of desired) {
      desiredById[wp.id] = true;
    }

    const features = mapGlobal.navWaypointFeatures;
    for (let i = features.getLength() - 1; i >= 0; i--) {
      const feat = features.item(i) as Feature<Point>;
      const id = feat.get("waypointId") as string;
      if (!desiredById[id] && id !== draggingWaypointId) {
        features.removeAt(i);
      }
    }

    const existingById: Record<string, Feature<Point>> = {};
    features.forEach((f) => {
      existingById[f.get("waypointId") as string] = f as Feature<Point>;
    });

    desired.forEach((wp, idx) => {
      const existing = existingById[wp.id];
      if (existing) {
        if (wp.id !== draggingWaypointId) {
          existing.setGeometry(new Point([wp.lng, wp.lat]));
        }
        existing.set("waypointIndex", idx);
        return;
      }
      features.push(
        new Feature({
          type: "navWaypoint",
          waypointId: wp.id,
          waypointIndex: idx,
          geometry: new Point([wp.lng, wp.lat]),
        })
      );
    });
  }

  let draggingWaypointId: string | null = null;
  let waypointModifyInteraction: Modify | null = null;
  let waypointModifySource: VectorSource | null = null;

  // The Modify interaction is added/removed alongside the "add waypoint" mode
  // toggle so dragging is only possible while the user has explicitly opted in
  // (this also avoids accidental drags during normal panning).
  $effect(() => {
    if (!mapGlobal.map) return;
    if (addWaypointActive && onMoveWaypoint && !waypointModifyInteraction) {
      installWaypointModify();
    } else if ((!addWaypointActive || !onMoveWaypoint) && waypointModifyInteraction) {
      uninstallWaypointModify();
    }
    if (!addWaypointActive && editPopupState.visible) {
      closeEditPopup();
    }
  });

  function installWaypointModify() {
    if (!mapGlobal.map || !waypointModifySource) return;
    waypointModifyInteraction = new Modify({
      source: waypointModifySource,
      // Restrict drags to the marker points: a route's line segments live in
      // a different layer/source so they can't be edited.
      pixelTolerance: 12,
    });
    waypointModifyInteraction.on("modifystart", (evt) => {
      const feat = evt.features.item(0) as Feature<Point> | undefined;
      draggingWaypointId = (feat?.get("waypointId") as string) ?? null;
    });
    waypointModifyInteraction.on("modifyend", (evt) => {
      const feat = evt.features.item(0) as Feature<Point> | undefined;
      draggingWaypointId = null;
      if (!feat || !onMoveWaypoint) return;
      const id = feat.get("waypointId") as string;
      const geom = feat.getGeometry();
      if (!id || !geom) return;
      const [lng, lat] = geom.getCoordinates();
      onMoveWaypoint(id, lat, lng);
    });
    mapGlobal.map.addInteraction(waypointModifyInteraction);
  }

  function uninstallWaypointModify() {
    if (mapGlobal.map && waypointModifyInteraction) {
      mapGlobal.map.removeInteraction(waypointModifyInteraction);
    }
    waypointModifyInteraction = null;
    draggingWaypointId = null;
  }

  // Returns the index of the segment in `coords` (i.e. the segment from
  // coords[i] to coords[i+1]) that lies closest to `pt`. Distances are in the
  // map's projected units, which is fine for "which one did I click" — we
  // only care about the relative ordering, not absolute meters.
  function closestSegmentIndex(coords: number[][], pt: number[]): number {
    let best = -1;
    let bestDist = Infinity;
    for (let i = 0; i < coords.length - 1; i++) {
      const d = pointSegmentDist(pt, coords[i], coords[i + 1]);
      if (d < bestDist) {
        bestDist = d;
        best = i;
      }
    }
    return best;
  }

  function pointSegmentDist(p: number[], a: number[], b: number[]): number {
    const dx = b[0] - a[0];
    const dy = b[1] - a[1];
    const lenSq = dx * dx + dy * dy;
    let t = lenSq === 0 ? 0 : ((p[0] - a[0]) * dx + (p[1] - a[1]) * dy) / lenSq;
    t = Math.max(0, Math.min(1, t));
    const cx = a[0] + t * dx;
    const cy = a[1] + t * dy;
    const ex = p[0] - cx;
    const ey = p[1] - cy;
    return Math.sqrt(ex * ex + ey * ey);
  }

  function showEditPopup(
    mode: "insert" | "delete",
    coord: number[],
    waypointId: string
  ) {
    editPopupState.mode = mode;
    editPopupState.lng = coord[0];
    editPopupState.lat = coord[1];
    editPopupState.waypointId = waypointId;
    editPopupState.visible = true;
    editPopupState.overlay?.setPosition(coord);
  }

  function closeEditPopup() {
    editPopupState.visible = false;
    editPopupState.overlay?.setPosition(undefined);
  }

  function confirmEditPopup() {
    if (editPopupState.mode === "insert") {
      onInsertWaypoint?.(editPopupState.waypointId, editPopupState.lat, editPopupState.lng);
    } else {
      onRemoveWaypoint?.(editPopupState.waypointId);
    }
    closeEditPopup();
  }

  function toggleHeadsUp() {
    headsUpActive = !headsUpActive;
    setCookie(COOKIE_HEADS_UP, headsUpActive ? "1" : "0", COOKIE_OPTS);
    applyHeadsUpRotation();
  }

  function toggleBoatPosition() {
    boatPositionMode = boatPositionMode === "center" ? "bottom" : "center";
    setCookie(COOKIE_BOAT_POSITION, boatPositionMode, COOKIE_OPTS);
    // Force re-center on next update by exiting pan mode and clearing locked state.
    mapInternalState.lastZoom = 0;
    mapInternalState.lastCenter = [0, 0];
    inPanMode = false;
    removeCookie(COOKIE_VIEW_CENTER, COOKIE_OPTS);
  }

  function updateHeadingLine() {
    mapGlobal.headingLineFeatures.clear();

    if (!myBoat) return;
    if (!isValidCoordinate(myBoat.location[0], myBoat.location[1])) return;
    if ((myBoat.speed ?? 0) <= 5) return;

    const start: [number, number] = [myBoat.location[1], myBoat.location[0]];
    const headingRad = (myBoat.heading * Math.PI) / 180;
    const nmInMeters = 1852;
    const lengthNm = headingLineLengthNm;

    const end = sphereOffset(start, lengthNm * nmInMeters, headingRad) as [number, number];
    mapGlobal.headingLineFeatures.push(
      new Feature({
        kind: "line",
        geometry: new LineString([start, end]),
      })
    );

    // Cross ticks every 1nm, perpendicular to the heading line.
    const tickHalfMeters = 75;
    const leftBearing = headingRad - Math.PI / 2;
    const rightBearing = headingRad + Math.PI / 2;
    for (let nm = 1; nm <= lengthNm; nm++) {
      const center = sphereOffset(start, nm * nmInMeters, headingRad) as [number, number];
      const left = sphereOffset(center, tickHalfMeters, leftBearing) as [number, number];
      const right = sphereOffset(center, tickHalfMeters, rightBearing) as [number, number];
      mapGlobal.headingLineFeatures.push(
        new Feature({
          kind: "tick",
          geometry: new LineString([left, right]),
        })
      );
    }
  }

  function applyHeadsUpRotation() {
    if (!mapGlobal.view) return;
    if (headsUpActive && myBoat) {
      mapGlobal.view.setRotation(-(myBoat.heading * Math.PI) / 180);
    } else {
      mapGlobal.view.setRotation(0);
    }
  }

  $effect(() => {
    // Re-apply rotation when boat heading changes while heads-up is active
    if (!headsUpActive) return;
    const _heading = myBoat?.heading;
    applyHeadsUpRotation();
  });

  function handleMeasureClick(evt: any) {
    const coord = evt.coordinate;

    if (measurePoints.length >= 2) {
      measurePoints = [coord];
      measureDistance = null;
      if (measureSource) measureSource.clear();
      const pointFeature = new Feature({ geometry: new Point(coord), type: "measure" });
      pointFeature.setStyle(
        new Style({
          image: new CircleStyle({
            radius: 6,
            fill: new Fill({ color: "#ff4444" }),
            stroke: new Stroke({ color: "white", width: 2 }),
          }),
        })
      );
      measureSource?.addFeature(pointFeature);
      return;
    }

    measurePoints = [...measurePoints, coord];

    const pointFeature = new Feature({ geometry: new Point(coord), type: "measure" });
    pointFeature.setStyle(
      new Style({
        image: new CircleStyle({
          radius: 6,
          fill: new Fill({ color: "#ff4444" }),
          stroke: new Stroke({ color: "white", width: 2 }),
        }),
      })
    );
    measureSource?.addFeature(pointFeature);

    if (measurePoints.length === 2) {
      const line = new Feature({ geometry: new LineString(measurePoints), type: "measure" });
      line.setStyle(
        new Style({
          stroke: new Stroke({ color: "#ff4444", width: 2, lineDash: [8, 4] }),
        })
      );
      measureSource?.addFeature(line);

      const meters = getDistance(measurePoints[0], measurePoints[1]);
      measureDistance = meters / 1852;
    }
  }

  function stopPanning() {
    // Don't touch zoom — the user's chosen zoom is preserved across pan
    // events and persisted via the change:resolution listener. We just
    // clear the pan-detection memory so the next tick re-centers on the
    // boat at the existing zoom.
    //
    // Also force-disable auto-zoom: once the user has zoomed/panned by
    // hand, the speed-formula would otherwise override their chosen zoom
    // on the very next tick. They can re-enable auto-zoom from the
    // toolbar button when they want it back.
    if (autoZoomActive) {
      autoZoomActive = false;
      setCookie(COOKIE_AUTO_ZOOM, "0", COOKIE_OPTS);
    }
    mapInternalState.lastZoom = 0;
    mapInternalState.lastCenter = [0, 0];
    inPanMode = false;
    removeCookie(COOKIE_VIEW_CENTER, COOKIE_OPTS);
  }

  function toggleAutoZoom() {
    autoZoomActive = !autoZoomActive;
    setCookie(COOKIE_AUTO_ZOOM, autoZoomActive ? "1" : "0", COOKIE_OPTS);
  }

  // showTileUrlForClick computes the XYZ tile that contains the clicked
  // lon/lat at the current view zoom and prints + copies its checkmate
  // tile URL (and the /compare URL). Used for diffing our render against
  // NOAA's WMS for a specific tile — toggle "tile URL" in the bottom
  // controls, click the map, paste the URL.
  function showTileUrlForClick(evt: any) {
    if (!mapGlobal.view) return;
    const coord = evt.coordinate as [number, number];
    if (!coord) return;
    const lon = coord[0];
    const lat = coord[1];
    const z = Math.round(mapGlobal.view.getZoom() ?? 0);
    const n = Math.pow(2, z);
    const x = Math.floor(((lon + 180) / 360) * n);
    const latRad = (lat * Math.PI) / 180;
    const y = Math.floor(
      ((1 - Math.log(Math.tan(latRad) + 1 / Math.cos(latRad)) / Math.PI) / 2) * n
    );
    const origin = tileBase || window.location.origin;
    const tileUrl = `${origin}/noaa-enc/tile/${z}/${x}/${y}.png`;
    const compareUrl = `${origin}/noaa-enc/compare/${z}/${x}/${y}.png`;
    const compareAllUrl = `${origin}/noaa-enc/compare/test?lat=${lat.toFixed(4)}&lon=${lon.toFixed(4)}`;
    console.log("tile:    ", tileUrl);
    console.log("compare: ", compareUrl);
    console.log("compare all zooms: ", compareAllUrl);
    if (navigator.clipboard) {
      void navigator.clipboard.writeText(tileUrl).catch(() => {});
    }
    // Cheap visible feedback. The popup overlay is already wired up for
    // boats, so reuse its element with our text — saves another overlay.
    const el = document.getElementById("tile-url-popup");
    if (el) {
      el.innerHTML = `<a href="${tileUrl}" target="_blank">tile z=${z} x=${x} y=${y}</a><br><a href="${compareUrl}" target="_blank">compare</a><br><a href="${compareAllUrl}" target="_blank">compare all zooms</a>`;
      el.style.display = "block";
      window.setTimeout(() => {
        el.style.display = "none";
      }, 4000);
    }
  }


  // tileBase is the origin of the map+weather server the app fetches tiles and
  // weather from. Empty = same-origin (this server). Set from /app-config at
  // startup (loadAppConfig), so an app on :8888 can pull tiles/weather from a
  // separate map server on :8989. api() prefixes every backend URL with it.
  let tileBase = "";
  function api(path: string): string {
    return `${tileBase}${path}`;
  }
  // Hosted chart server to fall back to when there's no /app-config at all
  // (e.g. a pure-static deploy with no module). Keeps the chart working instead
  // of requesting tiles from a blank same-origin host. When /app-config DOES
  // respond, its tileServerBaseURL wins — including "" which means "this server
  // serves tiles" (same-origin).
  const HOSTED_TILE_FALLBACK = "https://nycmaps.checkmatemaps.com";
  async function loadAppConfig(): Promise<void> {
    try {
      const resp = await fetch("/app-config");
      if (!resp.ok) {
        tileBase = HOSTED_TILE_FALLBACK;
        return;
      }
      const cfg = await resp.json();
      if (cfg && typeof cfg.tileServerBaseURL === "string") {
        tileBase = cfg.tileServerBaseURL.replace(/\/$/, "");
      }
    } catch {
      tileBase = HOSTED_TILE_FALLBACK;
    }
  }

  function setupLayers() {
    // Explicit zIndex per tile layer so OpenLayers renders in declaration
    // order regardless of toggle/insert sequence. Without this, toggling a
    // layer off and back on can land it on top of layers that should sit
    // above it (e.g. OSM ending up above checkmate after a reload).
    // core open street maps
    mapGlobal.layerOptions.push({
      name: "open street map",
      on: true,
      layer: new TileLayer({
        opacity: 1,
        preload: Infinity, // Preload all tiles at lower zoom levels
        zIndex: 1,
        source: new XYZ({
          // When a chart base is active, OSM is the under-chart fallback for
          // regions NOAA never charted. Skip the fetch (return undefined → no
          // request) for tiles fully inside US ENC coverage at z>=7, where the
          // opaque chart hides OSM anyway — so we only hit OSMF's tile servers
          // in foreign/uncharted waters. When OSM is the *selected* base
          // (no chart base active), load everything normally.
          tileUrlFunction: (tc) => {
            const z = tc[0],
              x = tc[1],
              y = tc[2];
            if (z >= 7 && chartBaseActive() && tileFullyInUSWaters(z, x, y)) {
              return undefined;
            }
            return `https://tile.openstreetmap.org/${z}/${x}/${y}.png`;
          },
          transition: 250, // Faster fade-in
        }),
      }),
    });

    // NOAA's public WMS chart service. Authoritative but slow — kept as a
    // fallback / comparison reference. When served from the Go module (or
    // proxied by Vite), we route through `/noaa-wms/proxy` so the disk cache
    // absorbs repeat tile fetches; otherwise we hit NOAA directly.
    const noaaWmsUrl = noaaCacheReachable()
      ? api("/noaa-wms/proxy")
      : "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer";
    // NOAA's public WMS as a manual reference/fallback layer (off by default).
    // The Checkmate chart below is always the default.
    mapGlobal.layerOptions.push({
      name: "noaa",
      on: false,
      minZoom: 7,
      layer: new TileLayer({
        opacity: 0.7,
        preload: 2,
        zIndex: 4,
        source: new TileWMS({
          url: noaaWmsUrl,
          // No _v cache-buster: NOAA tiles don't change with our build, and
          // including it makes every restart with a dirty tree (vite injects
          // a fresh Date.now() into __GIT_HASH__) generate a new disk-cache
          // key — so the proxy cache reads as empty after every restart.
          params: { LAYERS: "0,1,2,3,4,5,6" },
          transition: 300,
        }),
      }),
    });

    // Local ENC renderer — fast, lives at /noaa-enc/tile/{z}/{x}/{y}.png served
    // by the Go module on :8888 (and proxied through Vite on :5173). Only
    // registered when we're being served from one of those ports; elsewhere
    // the path doesn't exist.
    if (noaaCacheReachable()) {
      const sharedParams = new URLSearchParams();
      sharedParams.set("v", tileGenVersion);
      if (safeDepthParam) sharedParams.set("sd", safeDepthParam);

      // Cap on retained features per chart-overlay vector source.
      // bboxStrategy accumulates features as the user pans and never
      // evicts on its own; over a long coastal session that grew
      // without bound. When we cross the cap, schedule a refresh()
      // which clears features AND the loaded-extents tracking — the
      // current viewport then refetches on the next render tick.
      // Threshold tuned so it rarely triggers in normal harbour use
      // (a busy harbour view returns ~50-500 features per layer).
      const VECTOR_FEATURE_CAP = 3000;
      function capVectorSource(source: VectorSource<any>) {
        if (source.getFeatures().length > VECTOR_FEATURE_CAP) {
          // Defer to a microtask so we don't refresh mid-load — that
          // can race with OL's internal "I'm currently loading this
          // extent" bookkeeping.
          Promise.resolve().then(() => source.refresh());
        }
      }

      // Vector layer of navaid features (buoys, beacons, lights, daymarks).
      // Loaded from /noaa-enc/navaids on demand per visible bbox; rendered
      // as simple S-52-flavoured icons with a hover popup for full metadata.
      const navaidSource = new VectorSource({
        format: new GeoJSON(),
        strategy: bboxStrategy,
        loader: function (extent, _res, _proj, success, failure) {
          const [minLon, minLat, maxLon, maxLat] = extent;
          const url =
            api(`/noaa-enc/navaids?`) +
            `minLon=${minLon}&minLat=${minLat}` +
            `&maxLon=${maxLon}&maxLat=${maxLat}`;
          fetch(url)
            .then((r) => r.json())
            .then((j) => {
              const feats = navaidSource
                .getFormat()!
                .readFeatures(j) as Feature[];
              navaidSource.addFeatures(feats);
              success?.(feats);
              capVectorSource(navaidSource);
            })
            .catch((e) => {
              console.warn("navaids fetch failed", e);
              failure?.();
            });
        },
      });
      var navaidLayer = new Vector({
        source: navaidSource,
        style: navaidStyleFunction,
        zIndex: 7,
      });
      mapGlobal.navaidLayer = navaidLayer;
      // checkmate: tuned to mirror NOAA's WMS look as closely as possible.
      // Use this for offline use that should look interchangeable with the
      // live WMS layer. navaids=0 strips buoys/beacons/lights/daymarks from
      // the tile PNG — those render in the noaa-navaids OL vector layer
      // below so they can be interactive (hover for metadata).
      // landfill=0 drops LNDARE/BUAARE/BUISGL fills so the osm-detail
      // tile layer (zIndex 4) shows through where the chart says "land".
      // Vector layer of structure features (bridges, overhead cables,
      // overhead pipes, conveyors). Same pattern as navaids: GeoJSON
      // loaded per visible bbox; hover popup formats clearance + name.
      const structureSource = new VectorSource({
        format: new GeoJSON(),
        strategy: bboxStrategy,
        loader: function (extent, _res, _proj, success, failure) {
          const [minLon, minLat, maxLon, maxLat] = extent;
          const url =
            api(`/noaa-enc/structures?`) +
            `minLon=${minLon}&minLat=${minLat}` +
            `&maxLon=${maxLon}&maxLat=${maxLat}`;
          fetch(url)
            .then((r) => r.json())
            .then((j) => {
              const feats = structureSource
                .getFormat()!
                .readFeatures(j) as Feature[];
              structureSource.addFeatures(feats);
              success?.(feats);
              capVectorSource(structureSource);
            })
            .catch((e) => {
              console.warn("structures fetch failed", e);
              failure?.();
            });
        },
      });
      var structureLayer = new Vector({
        source: structureSource,
        style: structureStyleFunction,
        zIndex: 8,
      });
      mapGlobal.structureLayer = structureLayer;

      // Tile param variants, picked per-zoom by tileUrlFunction. The
      // rule: at each zoom we want exactly one source of truth for a
      // chart feature. Navaids and structures each have a vector layer
      // that turns on at their minZoom; the tile must bake the feature
      // *in* below that and skip it *out* at and above. We render this
      // as three variants — overview / mid / detail — picked by the
      // tightest minZoom of any vector layer the current zoom has
      // crossed.
      //
      // Bumping a layer's minZoom is now a one-line change here and on
      // the layer registration; no per-layer tileUrlFunction logic.
      const VECTOR_TILE_NAVAID_MIN_Z = 12;
      // The structures vector layer turns on at z=13 (hover icons), but
      // the tile keeps drawing structures through z=13 too — the user
      // wants the chart-style bridge rendering at that band, with the
      // hover icon overlaid. Only at z >= 14 do we cut the tile out and
      // let the vector layer be the sole renderer.
      const VECTOR_TILE_STRUCTURE_MIN_Z = 14;

      // Overview (z < navaidMin): ECDIS style, landfill off — everything
      // baked into the tile so the chart reads at coastal scale.
      const overviewParams = new URLSearchParams(sharedParams);
      overviewParams.set("style", "ecdis");
      overviewParams.set("landfill", "0");

      // Mid (navaidMin <= z < structureMin): navaids handled by the
      // vector layer; bridges/cables still baked into the tile so
      // they're visible in this band even though the structures vector
      // hasn't kicked in yet.
      const midParams = new URLSearchParams(sharedParams);
      midParams.set("style", "wms");
      midParams.set("navaids", "0");
      midParams.set("landfill", "0");

      // Detail (z >= structureMin): both vector layers active; tile
      // skips both classes so the on-screen feature is exactly one
      // hover-able icon per real-world object.
      const detailParams = new URLSearchParams(midParams);
      detailParams.set("skip", "BRIDGE,CBLOHD,PIPOHD,CONVYR");

      // The Checkmate chart: our merged ENC + OSM + weather raster, served from
      // the configured tile host (local module, a split tile server, or the
      // hosted default). Always the default chart layer.
      mapGlobal.layerOptions.push({
        name: "checkmate",
        displayName: "Checkmate",
        on: true,
        // Below z7 the Checkmate/NOAA charts have no useful detail and the
        // overview tiles are expensive to render; at that scale we show only
        // the public cloud OSM base layer (see the open-street-map suppression
        // in updateOnLayers, which is gated to z>=7 to match).
        minZoom: 7,
        layer: new TileLayer({
          opacity: 1,
          preload: 2,
          zIndex: 5,
          source: new XYZ({
            // Don't request (or preload) tiles below z7: the chart isn't shown
            // there (public OSM is), and a z5/z6 request triggers a ~10s NOAA
            // overview render server-side for a tile nobody sees. Without this,
            // preload:2 fetches z5/z6 backdrops while panning at z7.
            minZoom: 7,
            tileUrlFunction: (tileCoord) => {
              const [z, x, y] = tileCoord;
              const params =
                z >= VECTOR_TILE_STRUCTURE_MIN_Z
                  ? detailParams
                  : z >= VECTOR_TILE_NAVAID_MIN_Z
                    ? midParams
                    : overviewParams;
              return api(`/noaa-enc/tile/${z}/${x}/${y}.png?${params.toString()}`);
            },
            transition: 300,
          }),
        }),
      });

      // OSM detail (streets/buildings/land) is now composited INTO the
      // checkmate tile server-side (the merged ENC+OSM tile), so there's no
      // separate osm-detail layer anymore — that would draw OSM twice. The
      // standalone /noaa-enc/osm-tile endpoint still exists for debugging.
      mapGlobal.layerOptions.push({
        name: "noaa-navaids",
        displayName: "navaids",
        on: true,
        layer: navaidLayer,
        parent: "checkmate",
        // The tile bakes navaids in until it switches to midParams
        // (navaids=0) at tile-zoom >= VECTOR_TILE_NAVAID_MIN_Z. OpenLayers
        // picks the tile zoom by nearest RESOLUTION, so it starts loading those
        // z12 tiles once the fractional view zoom crosses the res-midpoint
        // between z11 and z12 — i.e. at viewZoom = 12 - log2(1.5) ≈ 11.415, not
        // 12 and not 11.5. This vector layer's gate is `viewZoom >= minZoom`,
        // so matching that exact crossover makes the vector navaids appear at
        // the same instant the baked navaids drop — no gap, no double-draw.
        minZoom: VECTOR_TILE_NAVAID_MIN_Z - Math.log2(1.5),
      });
      mapGlobal.layerOptions.push({
        name: "noaa-structures",
        displayName: "structures",
        on: true,
        layer: structureLayer,
        parent: "checkmate",
        // One zoom level tighter than navaids — bridges/cables are
        // denser in busy harbours and would clutter the overview a
        // step before the navaid icons start to.
        minZoom: 13,
      });
      // OpenSeaMap seamark overlay — global, crowd-sourced nautical marks
      // (buoys, beacons, lights, harbours) as a transparent raster overlay,
      // free + no API key + CORS-enabled. Complements the NOAA chart and, more
      // usefully, gives nautical detail in waters NOAA never charted. Standalone
      // (no parent) so it works over any base; default off; drawn above the
      // chart (zIndex 7). Attribution shown via the OL Attribution control.
      mapGlobal.layerOptions.push({
        name: "openseamap",
        displayName: "seamarks (OpenSeaMap)",
        on: false,
        layer: new TileLayer({
          opacity: 1,
          zIndex: 7,
          source: new XYZ({
            url: "https://tiles.openseamap.org/seamark/{z}/{x}/{y}.png",
            crossOrigin: "anonymous",
            maxZoom: 18,
            attributions:
              '© <a href="https://www.openseamap.org/" target="_blank" rel="noopener">OpenSeaMap</a> contributors',
            transition: 300,
          }),
        }),
      });

      // noaa-ecdis: same renderer + cells, but with S-52 conditional
      // symbology (DEPCNT02 bold safety contour, SOUNDG02 two-tone
      // soundings, TOPMAR rendering). Reads more like a real ECDIS display
      // and makes the boat-specific safety contour more visually obvious.
      const ecdisParams = new URLSearchParams(sharedParams);
      ecdisParams.set("style", "ecdis");
      mapGlobal.layerOptions.push({
        name: "noaa-ecdis",
        on: false,
        minZoom: 7,
        layer: new TileLayer({
          opacity: 0.7,
          preload: 2,
          zIndex: 6,
          source: new XYZ({
            // See checkmate: no tile requests below z7 (the chart isn't shown
            // there and a z5/z6 render is a wasted ~10s NOAA overview query).
            minZoom: 7,
            url: api(`/noaa-enc/tile/{z}/{x}/{y}.png?${ecdisParams.toString()}`),
            transition: 300,
          }),
        }),
      });

    }

    // Track layer for myBoat (child of boat layer)
    var trackLayer = new Vector({
      source: new VectorSource({
        features: mapGlobal.trackFeatures,
      }),
      style: createTrackStyleFunction("myBoat"),
      zIndex: 10,
    });

    // Store reference for style refreshing
    mapGlobal.trackLayer = trackLayer;

    // AIS Track layer (child of ais layer)
    var aisTrackLayer = new Vector({
      source: new VectorSource({
        features: mapGlobal.aisTrackFeatures,
      }),
      style: createTrackStyleFunction(""),
      zIndex: 10,
    });

    // Store reference for style refreshing
    mapGlobal.aisTrackLayer = aisTrackLayer;

    // by boat setup (only if myBoat is provided)
    if (myBoat) {
      mapGlobal.myBoatMarker = new Feature({
        type: "geoMarker",
        header: 0,
        geometry: new Point([0, 0]),
      });

      var myBoatFeatures = new Collection<Feature<Geometry>>();
      myBoatFeatures.push(mapGlobal.myBoatMarker);

      var myBoatLayer = new Vector({
        source: new VectorSource({
          features: myBoatFeatures,
        }),
        style: function (_feature) {
          const [sx, sy] = boatScaleAxes(myBoat.length, myBoat.beam);
          return createBoatStyle(
            myBoat.heading,
            [0.6 * sx, 0.6 * sy],
            effectiveVisibleBoats.has("myBoat"),
            myBoatImage
          );
        },
        zIndex: 100,
      });
      mapGlobal.layerOptions.push({
        name: "boat",
        on: true,
        layer: myBoatLayer,
      });

      // Track layer - child of boat
      mapGlobal.layerOptions.push({
        name: "track",
        on: true,
        layer: trackLayer,
        parent: "boat",
      });

      // Heading line layer - child of boat, default on when sog > 5 kt
      var headingLineLayer = new Vector({
        source: new VectorSource({
          features: mapGlobal.headingLineFeatures,
        }),
        style: function (feature) {
          const kind = feature.get("kind");
          if (kind === "tick") {
            return new Style({
              stroke: new Stroke({ color: "#000", width: 2 }),
            });
          }
          return new Style({
            stroke: new Stroke({ color: "#000", width: 2 }),
          });
        },
        zIndex: 25,
      });

      mapGlobal.layerOptions.push({
        name: "heading-line",
        displayName: "heading line",
        on: true,
        layer: headingLineLayer,
        parent: "boat",
      });

      // Route layer - child of boat, so only added when myBoat exists
      var routeLayer = new Vector({
        source: new VectorSource({
          features: mapGlobal.routeFeatures,
        }),
        style: new Style({
          stroke: new Stroke({
            color: "green",
            width: 3,
          }),
          fill: new Fill({
            color: "rgba(0, 255, 0, 0.1)",
          }),
        }),
        zIndex: 20,
      });

      mapGlobal.layerOptions.push({
        name: "route",
        on: true,
        layer: routeLayer,
        parent: "boat", // Route is a child of boat layer
      });

      // Nav-service route: line + waypoint markers driven by `navWaypoints`.
      // Two layers/sources (line and markers) so drag-to-edit only affects the
      // markers.
      var navRouteLineLayer = new Vector({
        source: new VectorSource({
          features: mapGlobal.navRouteLineFeatures,
        }),
        style: new Style({
          stroke: new Stroke({
            color: "#f59e0b",
            width: 3,
            lineDash: [10, 6],
          }),
        }),
        zIndex: 21,
      });

      waypointModifySource = new VectorSource({
        features: mapGlobal.navWaypointFeatures,
      });
      var navWaypointLayer = new Vector({
        source: waypointModifySource,
        style: new Style({
          image: new CircleStyle({
            radius: 7,
            fill: new Fill({ color: "#f59e0b" }),
            stroke: new Stroke({ color: "white", width: 2 }),
          }),
        }),
        zIndex: 22,
      });

      mapGlobal.layerOptions.push({
        name: "nav-route",
        displayName: "nav route",
        on: true,
        layer: navRouteLineLayer,
        parent: "boat",
      });
      mapGlobal.layerOptions.push({
        name: "nav-waypoints",
        displayName: "waypoints",
        on: true,
        layer: navWaypointLayer,
        parent: "boat",
      });
    }

    var aisLayer = new Vector({
      source: new VectorSource({
        features: mapGlobal.aisFeatures,
      }),
      style: aisStyleFunction,
      zIndex: 100,
    });
    mapGlobal.aisLayer = aisLayer;

    mapGlobal.layerOptions.push({
      name: "ais",
      on: true,
      layer: aisLayer,
    });

    // AIS Track layer - child of ais
    mapGlobal.layerOptions.push({
      name: "ais-track",
      displayName: "track",
      on: defaultAisVisible,
      layer: aisTrackLayer,
      parent: "ais",
    });

    // ais-projection: virtual sub-layer (no real OL layer). The
    // projection line is drawn inline by aisStyleFunction; this
    // toggle just gates that draw and the dropdown next to it picks
    // the projection length in minutes.
    mapGlobal.layerOptions.push({
      name: "ais-projection",
      displayName: "projection line",
      on: true,
      parent: "ais",
    });

    // Airstream toggle layer. Always registered (resource discovery is
    // async and setupLayers only runs once at mount, so we can't gate
    // registration on airstreamConfigured being true at this moment).
    // The layer panel hides the row when airstreamConfigured is false,
    // and the bbox-emit / DoCommand callbacks check the prop themselves —
    // so a machine without airstream sees no toggle and fires nothing.
    mapGlobal.layerOptions.push({
      name: "airstream",
      displayName: "airstream",
      on: false,
      layer: new Vector({ source: new VectorSource() }),
    });
  }

  function findLayerByName(name: string): LayerOption | null {
    for (var l of mapGlobal.layerOptions) {
      if (l.name == name) {
        return l;
      }
    }
    return null;
  }

  // The Go caching proxy is only mounted on the module's own HTTP server (default port
  // 8888) and is also reachable through the Vite dev server (5173) via its proxy. When
  // the page is served from anywhere else we skip both the proxy URL and the prefetch
  // calls and let OpenLayers hit NOAA directly.
  // Whether the Go module's noaa-enc + noaa-wms endpoints are reachable on
  // the current origin. Probed once at mount via /noaa-wms/stats; falls
  // back to the legacy port heuristic for environments where the probe
  // hasn't completed yet (so layer setup at first render still picks the
  // right URL when running locally on :5173 / :8888).
  let noaaCacheProbeResult = $state<boolean | null>(null);

  function noaaCacheReachable(): boolean {
    if (noaaCacheProbeResult !== null) return noaaCacheProbeResult;
    if (typeof window === "undefined") return false;
    const port = window.location.port;
    return port === "5173" || port === "8888";
  }

  async function probeNoaaCache() {
    try {
      const resp = await fetch(api("/noaa-wms/stats"), { method: "GET" });
      noaaCacheProbeResult = resp.ok;
    } catch {
      noaaCacheProbeResult = false;
    }
  }

  // Last bbox we emitted to onAirstreamBboxChange so trivial pans don't
  // re-fire the callback. Rounded to keep this a coarse comparison.
  let lastAirstreamBboxKey = "";

  // After a zoom (or any view settle), put the boat back at its configured
  // anchor pixel — center for "center" mode, 80% down for "bottom" mode.
  // OL's default scroll/pinch zoom anchors at the cursor, which would
  // otherwise drift the boat off-anchor when the user just wanted to
  // change zoom level. Skipped while in pan mode (the user is intentionally
  // looking elsewhere) and when there's no usable boat fix.
  function maybeReanchorOnBoat() {
    if (inPanMode) return;
    if (!myBoat || !myBoat.location) return;
    if (myBoat.location[0] === 0 && myBoat.location[1] === 0) return;
    if (!mapGlobal.map || !mapGlobal.view) return;
    const sz = mapGlobal.map.getSize();
    if (!sz) return;
    const pp = [myBoat.location[1], myBoat.location[0]];
    const boatPx: [number, number] =
      boatPositionMode === "bottom" ? [sz[0] / 2, sz[1] * 0.8] : [sz[0] / 2, sz[1] / 2];
    mapGlobal.view.centerOn(pp, sz, boatPx);
    // Keep mapInternalState.lastCenter in sync with the now-anchored view
    // so updateFromData's pan-detection diff doesn't false-positive on
    // the next tick.
    const vc = mapGlobal.view.getCenter();
    if (vc) mapInternalState.lastCenter = [vc[0], vc[1]];
  }

  function maybeEmitAirstreamBbox() {
    if (!airstreamConfigured || !onAirstreamBboxChange) return;
    const layer = findLayerByName("airstream");
    if (!layer || !layer.on) return;
    if (!mapGlobal.map || !mapGlobal.view) return;
    const size = mapGlobal.map.getSize();
    if (!size) return;
    const extent = mapGlobal.view.calculateExtent(size);
    // Round to 0.01° (~1 km) so a tiny drift doesn't churn the airstream
    // websocket. set_bounding_box drops and reconnects on every call.
    const round = (n: number) => Math.round(n * 100) / 100;
    const minLon = round(extent[0]);
    const minLat = round(extent[1]);
    const maxLon = round(extent[2]);
    const maxLat = round(extent[3]);
    if (!Number.isFinite(minLon) || minLat >= maxLat || minLon >= maxLon) return;
    const key = `${minLon},${minLat},${maxLon},${maxLat}`;
    if (key === lastAirstreamBboxKey) return;
    lastAirstreamBboxKey = key;
    onAirstreamBboxChange({ minLon, minLat, maxLon, maxLat });
  }


  function findOnLayerIndexOfName(name: string): number {
    var l = findLayerByName(name);
    if (l == null) {
      return -2;
    }

    for (var i = 0; i < mapGlobal.onLayers.getLength(); i++) {
      if ((mapGlobal.onLayers.item(i) as any).ol_uid == (l.layer as any).ol_uid) {
        return i;
      }
    }
    return -1;
  }

  function updateOnLayers() {
    // When the popup is open on an AIS target, force the AIS-track layer on
    // so the selected boat's history shows even if the user has the toggle
    // off — the per-feature filter in createTrackStyleFunction hides the
    // other boats' tracks, so only the selected one renders.
    const aisPopupForceTrack =
      popupState.visible &&
      !popupState.content.isMyBoat &&
      !!popupState.content.mmsi;
    const myBoatPopupForceTrack =
      popupState.visible && popupState.content.isMyBoat;

    // When a chart base (checkmate / noaa / noaa-ecdis) is active, force the
    // public OSM base on (it's the radio-"off" sibling) at ALL zooms as a
    // fallback: below z7 it's the sole base (chart hidden), and at z>=7 it sits
    // UNDER the chart so regions NOAA never charted — where the merged tile is
    // fully transparent — show OSM land/water/coastline instead of blank white.
    // Covered tiles render fully opaque (no bleed), and the OSM base's
    // tileUrlFunction skips the actual fetch inside US ENC coverage, so we only
    // hit OSMF's servers in foreign/uncharted waters.
    const osmChartFallback = chartBaseActive();
    // The OSM base's tileUrlFunction skips US tiles only while a chart base is
    // active; when that flips (user switches base), refresh so previously
    // skipped/loaded tiles re-evaluate.
    if (osmChartFallback !== prevChartBaseActive) {
      prevChartBaseActive = osmChartFallback;
      const osmBase = mapGlobal.layerOptions.find(
        (l) => l.name === "open street map",
      );
      (osmBase?.layer as TileLayer<XYZ> | undefined)?.getSource()?.refresh();
    }

    for (var l of mapGlobal.layerOptions) {
      // Virtual layers (no `layer` field, e.g. ais-projection) are
      // gated by the parent's style function and never added to the
      // map directly. When their toggle changes the parent layer
      // re-renders via the $effect below.
      if (!l.layer) continue;

      var idx = findOnLayerIndexOfName(l.name);

      // Check if parent layer exists and is off
      const parentLayer = l.parent ? mapGlobal.layerOptions.find((p) => p.name === l.parent) : null;
      const isParentOff = parentLayer && !parentLayer.on;

      const popupForcesOn =
        (l.name === "ais-track" && aisPopupForceTrack) ||
        (l.name === "track" && myBoatPopupForceTrack);

      // Force the (radio-"off") OSM base on under a selected chart base at all
      // zooms, so uncharted regions show OSM instead of blank white (see
      // osmChartFallback above). No suppression needed: covered tiles render
      // fully opaque, so the base only shows through where the chart is blank.
      const osmForcesOn = l.name === "open street map" && osmChartFallback;

      // Layer should be visible only if it's on AND (has no parent OR parent is on)
      const shouldBeVisible =
        (l.on || popupForcesOn || osmForcesOn) && !isParentOff;

      if (shouldBeVisible) {
        if (idx < 0) {
          // Insert tile layers before vector layers to ensure proper z-ordering
          // Vector layers (boat, ais, track, route) should always be on top
          if (l.layer instanceof TileLayer) {
            // Find the first vector layer index and insert before it
            let insertIdx = 0;
            for (let i = 0; i < mapGlobal.onLayers.getLength(); i++) {
              if (mapGlobal.onLayers.item(i) instanceof Vector) {
                break;
              }
              insertIdx = i + 1;
            }
            mapGlobal.onLayers.insertAt(insertIdx, l.layer);
          } else {
            // Vector layers go on top
            mapGlobal.onLayers.push(l.layer);
          }
        }
      } else {
        if (idx >= 0) {
          mapGlobal.onLayers.removeAt(idx);
        }
      }
    }
  }

  function pointDiff(x: number[], y: number[]): number {
    var a = x[0] - y[0];
    var b = x[1] - y[1];
    var c = a * a + b * b;
    return Math.sqrt(c);
  }

  // Store event handler references for cleanup (outside setupMap so they're accessible in onMount cleanup)
  let mapClickHandler: any = null;
  let mapPointerHandler: any = null;
  let mapPointerDragHandler: any = null;

  async function setupMap() {
    useGeographic();
    // Probe before setupLayers so the checkmate / noaa-ecdis / noaa-wms
    // layers register with the correct origin assumption regardless of
    // which port the Go module is bound to.
    // Resolve where to fetch tiles/weather (same-origin or a separate map
    // server) first, so the cache probe and every layer URL use the right base.
    await loadAppConfig();
    await probeNoaaCache();
    setupLayers();

    const savedCenter = loadViewCenter();
    mapGlobal.view = new View({
      center: savedCenter ?? DEFAULT_VIEW_CENTER,
      zoom: loadViewZoom(),
      maxZoom: 19,
    });
    // Restoring a manual pan position implies the user was browsing away from
    // the boat — keep that mode on reload so the boat tracker doesn't yank
    // the view back the moment a fix arrives.
    if (savedCenter) {
      inPanMode = true;
    }

    // Persist whatever zoom the view ends up at so reloads come back at
    // the same level. change:resolution fires for both user-initiated
    // changes (wheel/pinch) and our own setZoom calls in auto-zoom mode —
    // both are correct things to remember.
    // Zoom-gated visibility: each LayerOption can declare a minZoom;
    // when the current zoom is below that, the underlying OL layer is
    // hidden regardless of the user's on/off toggle. Re-run on every
    // resolution change so panning through scales updates naturally,
    // and once at startup so the gate is applied on first paint.
    const applyZoomGates = () => {
      const z = mapGlobal.view?.getZoom();
      if (typeof z !== "number") return;
      for (const l of mapGlobal.layerOptions) {
        if (!l.layer) continue;
        if (typeof l.minZoom !== "number" && typeof l.maxZoom !== "number") {
          continue;
        }
        const minOK = typeof l.minZoom !== "number" || z >= l.minZoom;
        const maxOK = typeof l.maxZoom !== "number" || z <= l.maxZoom;
        l.layer.setVisible(minOK && maxOK);
      }
    };
    let lastOverview = (mapGlobal.view.getZoom() ?? 0) < 7;
    mapGlobal.view.on("change:resolution", () => {
      const z = mapGlobal.view?.getZoom();
      if (typeof z === "number" && Number.isFinite(z)) {
        setCookie(COOKIE_VIEW_ZOOM, String(z), COOKIE_OPTS);
        currentZoom = z;
      }
      applyZoomGates();
      // Crossing the z7 boundary flips whether the OSM base is suppressed by
      // checkmate (updateOnLayers only re-runs on toggles, not on zoom), so
      // re-run it on a crossing to add/remove the OSM base layer accordingly.
      const overview = (z ?? 0) < 7;
      if (overview !== lastOverview) {
        lastOverview = overview;
        updateOnLayers();
      }
    });
    const z0 = mapGlobal.view.getZoom();
    if (typeof z0 === "number" && Number.isFinite(z0)) currentZoom = z0;
    applyZoomGates();

    updateOnLayers();
    updateOnLayers();

    var scaleThing = new ScaleLine({
      units: "nautical",
      bar: true,
      text: false,
      //minWidth: 140,
    });

    mapGlobal.map = new Map({
      target: "map",
      layers: mapGlobal.onLayers as Collection<BaseLayer>,
      view: mapGlobal.view,
      controls: defaultControls().extend([scaleThing]),
    });

    // Replace the default mouse-wheel zoom with one that anchors at the
    // boat's current position (so the boat stays fixed on screen while
    // surrounding chart zooms around it). We subclass OL's MouseWheelZoom
    // and rewrite the event coordinate before super.handleEvent runs;
    // that's the value the parent records as its lastAnchor_, so the
    // wheel/trackpad detection, debouncing, and animation tweening all
    // come along for free — we just point them at the boat instead of
    // the cursor. Falls back to the original (cursor) coordinate when the
    // user is in pan mode or has no usable boat fix.
    {
      const interactions = mapGlobal.map.getInteractions();
      const existing = interactions.getArray().slice();
      for (const item of existing) {
        if (item instanceof MouseWheelZoom) {
          interactions.remove(item);
        }
      }
      class BoatAnchoredMouseWheelZoom extends MouseWheelZoom {
        handleEvent(event: any) {
          if (event && event.type === "wheel") {
            // Boat-anchor when auto-tracking (not pan mode): the chart
            // is following the boat, so zoom-around-boat keeps the boat
            // visually fixed during the zoom rather than letting OL's
            // default cursor anchor drag the boat off its anchor pixel
            // for a frame before the next tick re-centers. In pan mode
            // we let OL's default cursor anchor run — the user is
            // exploring elsewhere and "zoom toward what I'm pointing
            // at" is the expected behaviour for a normal map.
            if (
              !inPanMode &&
              myBoat?.location &&
              !(myBoat.location[0] === 0 && myBoat.location[1] === 0)
            ) {
              const map = event.map ?? mapGlobal.map;
              const px = map?.getPixelFromCoordinate([
                myBoat.location[1],
                myBoat.location[0],
              ]);
              const sz = map?.getSize();
              if (px && sz && px[0] >= 0 && px[1] >= 0 && px[0] <= sz[0] && px[1] <= sz[1]) {
                event.pixel = px;
              }
            }
          }
          return super.handleEvent(event);
        }
      }
      mapGlobal.map.addInteraction(new BoatAnchoredMouseWheelZoom());
    }

    mapGlobal.map.on("moveend", () => {
      maybeEmitAirstreamBbox();
      maybeReanchorOnBoat();
      // Only persist when the user is intentionally off-boat; otherwise the
      // boat-follow tracker would constantly overwrite the cookie with the
      // current boat position.
      if (inPanMode) {
        const c = mapGlobal.view?.getCenter();
        if (c && Number.isFinite(c[0]) && Number.isFinite(c[1])) {
          setCookie(COOKIE_VIEW_CENTER, JSON.stringify([c[0], c[1]]), COOKIE_OPTS);
        }
      }
    });

    // Setup popup overlay
    const popupElement = document.getElementById("boat-popup");
    popupState.overlay = new Overlay({
      element: popupElement || undefined,
      autoPan: false,
      positioning: "bottom-center",
      offset: [0, -15],
    });
    mapGlobal.map.addOverlay(popupState.overlay);

    // Setup edit popup overlay (single popup, anchored above the clicked
    // spot on the route line or a waypoint marker).
    const editPopupEl = document.getElementById("edit-waypoint-popup");
    editPopupState.overlay = new Overlay({
      element: editPopupEl || undefined,
      autoPan: false,
      positioning: "bottom-center",
      offset: [0, -14],
      stopEvent: true,
    });
    mapGlobal.map.addOverlay(editPopupState.overlay);

    // Setup depth tooltip overlay
    const depthTooltipElement = document.getElementById("depth-tooltip");
    const depthTooltipOverlay = new Overlay({
      element: depthTooltipElement || undefined,
      positioning: "bottom-center",
      offset: [0, -10],
    });
    mapGlobal.map.addOverlay(depthTooltipOverlay);

    // Setup navaid hover tooltip overlay
    const navaidTooltipElement = document.getElementById("navaid-tooltip");
    const navaidTooltipOverlay = new Overlay({
      element: navaidTooltipElement || undefined,
      positioning: "bottom-center",
      offset: [0, -12],
    });
    mapGlobal.map.addOverlay(navaidTooltipOverlay);

    // Setup my-boat track-time tooltip overlay (mirrors depth tooltip).
    // Hovering over a my-boat track segment shows when the boat passed
    // through that point.
    const trackTimeTooltipElement = document.getElementById("track-time-tooltip");
    const trackTimeTooltipOverlay = new Overlay({
      element: trackTimeTooltipElement || undefined,
      positioning: "bottom-center",
      offset: [0, -10],
    });
    mapGlobal.map.addOverlay(trackTimeTooltipOverlay);

    // AIS hover tooltip — shows the vessel's flag of registration (derived
    // from the MMSI's MID) and country name. The full popup still opens
    // on click; this is the at-a-glance "who's that" answer.
    const aisTooltipElement = document.getElementById("ais-tooltip");
    const aisTooltipOverlay = new Overlay({
      element: aisTooltipElement || undefined,
      positioning: "bottom-center",
      offset: [0, -18],
    });
    mapGlobal.map.addOverlay(aisTooltipOverlay);


    // Setup measure layer
    measureSource = new VectorSource();
    const measureLayer = new Vector({
      source: measureSource,
      zIndex: 9999,
    });
    mapGlobal.map.addLayer(measureLayer);

    mapClickHandler = function (evt: any) {
      if (tileUrlActive) {
        showTileUrlForClick(evt);
        return;
      }
      if (measureActive) {
        handleMeasureClick(evt);
        return;
      }
      if (addWaypointActive && onAddWaypoint) {
        // Hit-test in priority order: existing waypoint marker → route line →
        // empty water. Iterate features ourselves so we can find the *first*
        // marker AND the *first* line under the pixel without re-querying.
        let waypointFeature: Feature<Point> | null = null;
        let lineFeature: Feature<LineString> | null = null;
        mapGlobal.map!.forEachFeatureAtPixel(
          evt.pixel,
          (f) => {
            const t = f.get("type");
            if (!waypointFeature && t === "navWaypoint") {
              waypointFeature = f as Feature<Point>;
            } else if (!lineFeature && t === "navRoute") {
              lineFeature = f as Feature<LineString>;
            }
            // Stop early once we have both potential candidates.
            return waypointFeature && lineFeature ? true : undefined;
          },
          { hitTolerance: 8 }
        );

        if (waypointFeature && onRemoveWaypoint) {
          const wpFeat = waypointFeature as Feature<Point>;
          const id = wpFeat.get("waypointId") as string;
          const geom = wpFeat.getGeometry();
          if (id && geom) {
            showEditPopup("delete", geom.getCoordinates(), id);
            return;
          }
        }

        if (lineFeature && onInsertWaypoint && navWaypoints && navWaypoints.length > 0) {
          const lineFeat = lineFeature as Feature<LineString>;
          const geom = lineFeat.getGeometry();
          const segIds = lineFeat.get("segmentBeforeIds") as string[] | undefined;
          if (geom && segIds && segIds.length > 0) {
            const segIdx = closestSegmentIndex(geom.getCoordinates(), evt.coordinate);
            const beforeId = segIds[Math.max(0, Math.min(segIds.length - 1, segIdx))];
            showEditPopup("insert", evt.coordinate, beforeId);
            return;
          }
        }

        // Empty water in edit mode: append to the end of the route.
        const [lng, lat] = evt.coordinate;
        onAddWaypoint(lat, lng);
        return;
      }
      // Clicking outside any feature dismisses the edit popup if it's open.
      if (editPopupState.visible) {
        closeEditPopup();
      }
      const feature = mapGlobal.map!.forEachFeatureAtPixel(evt.pixel, function (f) {
        const type = f.get("type");
        if (type === "ais" || type === "geoMarker" || type === "detection") {
          return f;
        }
        return null;
      });

      if (feature) {
        const type = feature.get("type");

        if (type === "detection") {
          const detectionData = feature.get("detectionData");
          if (detectionData && detectionConfig?.onClick) {
            detectionConfig.onClick(detectionData);
          }
          return;
        }

        const geom = feature.getGeometry() as Point | undefined;
        if (!geom) return;
        const coords = geom.getCoordinates();

        if (type === "geoMarker" && myBoat) {
          popupState.content = {
            name: "My Boat",
            mmsi: "",
            speed: myBoat.speed,
            heading: myBoat.heading,
            cog: myBoat.cog,
            lat: coords[1],
            lng: coords[0],
            isMyBoat: true,
            host: myBoat.host,
            partId: myBoat.partId,
            isOnline: myBoat.isOnline ?? true,
          };
        } else if (type === "ais") {
          const mmsi = feature.get("mmsi") || "";
          const boat = boats?.find((b) => b.mmsi === mmsi);
          let cpaNm: number | null = null;
          let tcpaMin: number | null = null;
          if (
            boat &&
            myBoat &&
            myBoat.location &&
            !(myBoat.location[0] === 0 && myBoat.location[1] === 0)
          ) {
            const r = computeCpa(
              myBoat.location[0],
              myBoat.location[1],
              myBoat.heading,
              myBoat.speed,
              boat.location[0],
              boat.location[1],
              boat.cog ?? boat.heading,
              boat.speed
            );
            if (r) {
              cpaNm = r.cpaNm;
              tcpaMin = r.tcpaMin;
            }
          }
          popupState.content = {
            name: feature.get("name") || "Unknown",
            mmsi,
            speed: feature.get("speed") || 0,
            heading: feature.get("heading") || 0,
            cog: feature.get("cog"),
            lat: coords[1],
            lng: coords[0],
            isMyBoat: false,
            host: boat?.host,
            partId: boat?.partId,
            isOnline: boat?.isOnline ?? false,
            length: boat?.length,
            destination: boat?.destination,
            cpaNm,
            tcpaMin,
          };
        }
        popupState.visible = true;
        popupState.overlay?.setPosition(coords);

        const boatPartId = popupState.content.partId || popupState.content.mmsi;
        onBoatPopupOpen?.(boatPartId);
      } else {
        closePopup();
      }
    };
    mapGlobal.map.on("click", mapClickHandler);

    // Change cursor on hover over boats + show depth tooltip on track hover
    mapPointerHandler = function (evt: any) {
      const hit = mapGlobal.map!.hasFeatureAtPixel(evt.pixel, {
        layerFilter: (layer) => {
          return (
            (layer as any)
              .getSource()
              ?.getFeatures?.()
              ?.some?.(
                (f: Feature) =>
                  f.get("type") === "ais" ||
                  f.get("type") === "geoMarker" ||
                  f.get("type") === "detection"
              ) ?? false
          );
        },
      });
      mapGlobal.map!.getTargetElement()!.style.cursor =
        measureActive || addWaypointActive ? "crosshair" : hit ? "pointer" : "";

      // Depth tooltip on track hover
      let depthFound = false;
      if (depthColorTrack) {
        mapGlobal.map!.forEachFeatureAtPixel(
          evt.pixel,
          (feature) => {
            const depth = feature.get("depth");
            if (depth !== undefined && depth !== null && !depthFound) {
              depthFound = true;
              if (depthTooltipElement) {
                depthTooltipElement.textContent = depth.toFixed(1) + " ft";
              }
              depthTooltipOverlay.setPosition(evt.coordinate);
            }
          },
          { hitTolerance: 3 }
        );
      }
      if (!depthFound) {
        depthTooltipOverlay.setPosition(undefined);
      }

      // Navaid + structure hover tooltip. Both layers share one tooltip
      // element so they don't fight for screen space; the layer the
      // feature came from selects which formatter (navaid vs bridge/
      // overhead-structure) runs.
      let chartFeatureFound = false;
      if (mapGlobal.navaidLayer || mapGlobal.structureLayer) {
        mapGlobal.map!.forEachFeatureAtPixel(
          evt.pixel,
          (feature, layer) => {
            if (chartFeatureFound) return;
            const props = (feature as Feature).getProperties();
            if (!props || !props.class) return;
            // Backend-flagged uninformative / duplicate-icon structures
            // still draw a trace line but have no icon — the canonical
            // same-named entry carries the popup. Skip them here so
            // hovering the line doesn't open an empty (or redundant)
            // tooltip.
            if (props.uninformative === true || props.hideIcon === true) return;
            chartFeatureFound = true;
            const isStructure = layer === mapGlobal.structureLayer;
            if (navaidTooltipElement) {
              navaidTooltipElement.innerHTML = isStructure
                ? formatStructureTooltip(props)
                : formatNavaidTooltip(props);
            }
            const geom = (feature as Feature).getGeometry();
            if (geom && geom.getType() === "Point") {
              navaidTooltipOverlay.setPosition(
                (geom as Point).getCoordinates()
              );
            } else if (geom) {
              // Line/polygon (typical for bridges): anchor at the cursor
              // so the tooltip tracks the hover point rather than the
              // feature's first vertex.
              navaidTooltipOverlay.setPosition(evt.coordinate);
            }
          },
          {
            hitTolerance: 4,
            layerFilter: (layer) =>
              layer === mapGlobal.navaidLayer ||
              layer === mapGlobal.structureLayer,
          }
        );
      }
      if (!chartFeatureFound) {
        navaidTooltipOverlay.setPosition(undefined);
      }

      // AIS hover tooltip — vessel name only. Country/flag lives in the
      // click popup. Pinned to the vessel position so it doesn't jitter
      // with the cursor.
      let aisFound = false;
      mapGlobal.map!.forEachFeatureAtPixel(
        evt.pixel,
        (feature) => {
          if (aisFound) return;
          if (feature.get("type") !== "ais") return;
          const name = (feature.get("name") as string | undefined) || "";
          const mmsi = feature.get("mmsi") as string | undefined;
          const label = name.trim() || mmsi || "";
          if (!label) return;
          aisFound = true;
          if (aisTooltipElement) {
            aisTooltipElement.textContent = label;
          }
          const geom = (feature as Feature).getGeometry();
          if (geom && geom.getType() === "Point") {
            aisTooltipOverlay.setPosition((geom as Point).getCoordinates());
          } else {
            aisTooltipOverlay.setPosition(evt.coordinate);
          }
        },
        { hitTolerance: 3 }
      );
      if (!aisFound) {
        aisTooltipOverlay.setPosition(undefined);
      }

      // My-boat track-time tooltip: if the cursor is on a segment of the
      // user's own track, show when the boat was at that point. AIS
      // tracks are skipped — the user asked for "my track line" only.
      let timeFound = false;
      mapGlobal.map!.forEachFeatureAtPixel(
        evt.pixel,
        (feature) => {
          if (timeFound) return;
          if (feature.get("boatId") !== "myBoat") return;
          const ts = feature.get("ts");
          if (typeof ts !== "number") return;
          timeFound = true;
          if (trackTimeTooltipElement) {
            trackTimeTooltipElement.textContent = formatTrackTime(ts);
          }
          trackTimeTooltipOverlay.setPosition(evt.coordinate);
        },
        { hitTolerance: 3 }
      );
      if (!timeFound) {
        trackTimeTooltipOverlay.setPosition(undefined);
      }

      // Cursor-info: GPS position of the mouse, plus distance + bearing
      // from boat to mouse position when we have a usable boat fix.
      // evt.coordinate is [lng, lat] under useGeographic(); our helpers
      // want [lat, lng].
      if (evt.coordinate) {
        const cursorLngLat = evt.coordinate as [number, number];
        const cursorLatLng: [number, number] = [cursorLngLat[1], cursorLngLat[0]];
        let nm: number | null = null;
        let brg: number | null = null;
        if (
          myBoat &&
          myBoat.location &&
          !(myBoat.location[0] === 0 && myBoat.location[1] === 0)
        ) {
          const boatLatLng: [number, number] = [myBoat.location[0], myBoat.location[1]];
          const meters = getDistance(
            [myBoat.location[1], myBoat.location[0]], // [lng, lat]
            cursorLngLat
          );
          nm = meters / 1852;
          brg = bearingDeg(boatLatLng, cursorLatLng);
        }
        // Wind/wave sampling lives in WeatherOverlays (which owns the
        // handles); it publishes cursorSampler back to us. Read it at call
        // time so we always see the latest closure over the child's state.
        const { windKt, windFromDeg, waveM, waveFromDeg } =
          cursorSampler?.(cursorLngLat[0], cursorLngLat[1]) ?? {
            windKt: null,
            windFromDeg: null,
            waveM: null,
            waveFromDeg: null,
          };
        cursorInfo = {
          lat: cursorLatLng[0],
          lng: cursorLatLng[1],
          nm,
          brg,
          windKt,
          windFromDeg,
          waveM,
          waveFromDeg,
        };
      } else {
        cursorInfo = null;
      }
    };
    mapGlobal.map.on("pointermove", mapPointerHandler);
    // Hide the cursor-info box when the pointer leaves the map entirely.
    const target = mapGlobal.map.getTargetElement();
    if (target) {
      target.addEventListener("pointerleave", () => {
        cursorInfo = null;
      });
    }

    // Pointer-drag handling with a pixel threshold. OL fires `pointerdrag`
    // for any pointer-with-button-pressed movement, including sub-pixel
    // jitter and stray touchscreen contact. Without a threshold the user
    // would see "Stop Panning" reappear minutes after dismissing it because
    // a single stray drag event flipped inPanMode. We treat a drag as
    // intentional only after the cumulative distance from pointerdown
    // exceeds dragPxThreshold.
    let pointerDownPx: [number, number] | null = null;
    let pointerDragCounted = false;
    const dragPxThreshold = 10;
    mapGlobal.map.on("pointerdown", (evt: any) => {
      pointerDownPx = evt.pixel as [number, number];
      pointerDragCounted = false;
    });
    mapPointerDragHandler = function (evt: any) {
      if (pointerDragCounted) return;
      const px = evt.pixel as [number, number] | undefined;
      if (!px) {
        // No pixel info — fall back to the previous behaviour so we don't
        // miss a real drag.
        inPanMode = true;
        pointerDragCounted = true;
        return;
      }
      if (!pointerDownPx) {
        // Missed the pointerdown for some reason; treat the first observed
        // drag pixel as the anchor and decide on the next event.
        pointerDownPx = px;
        return;
      }
      const dx = px[0] - pointerDownPx[0];
      const dy = px[1] - pointerDownPx[1];
      if (dx * dx + dy * dy >= dragPxThreshold * dragPxThreshold) {
        inPanMode = true;
        pointerDragCounted = true;
      }
    };
    mapGlobal.map.on("pointerdrag", mapPointerDragHandler);

    console.log("setupMap finished");

    // Initial fit to show all boats with room for popups (only when boats panel enabled)
    setTimeout(() => {
      mapGlobal.map?.updateSize(); // Ensure map has correct dimensions

      // Restore saved on/off state for known layers from cookie
      var savedLayers = loadSavedLayerStates();
      for (var i = 0; i < mapGlobal.layerOptions.length; i++) {
        var name = mapGlobal.layerOptions[i].name;
        if (Object.prototype.hasOwnProperty.call(savedLayers, name)) {
          mapGlobal.layerOptions[i].on = !!savedLayers[name];
        }
      }

      if (enableBoatsPanel && boats && boats.length > 0) {
        fitToVisibleBoats();
      }
      // Expose API to parent component
      onReady?.({
        fitToVisibleBoats,
        selectAllBoats,
        deselectAllBoats,
        setVisibleBoats,
        getVisibleBoats: () => new Set(visibleBoats),
        setDetections: (detections: Detection[] | undefined) => {
          currentDetections = detections;
        },
        focusBoat,
      });
    }, 100);
  }

  function closePopup() {
    popupState.visible = false;
    if (popupState.overlay) {
      popupState.overlay.setPosition(undefined);
    }
  }

  // Focus a boat by mmsi/partId: make it visible even if offline, fly to it, open popup.
  function focusBoat(mmsi: string) {
    // Ensure the boat is in the visible set
    if (!visibleBoats.has(mmsi)) {
      visibleBoats = new Set([...visibleBoats, mmsi]);
    }

    const boat = boats?.find((b) => b.mmsi === mmsi);
    if (!boat) return;

    const coords: [number, number] = [boat.location[1], boat.location[0]];

    if (mapGlobal.view) {
      mapGlobal.view.animate({
        center: coords,
        zoom: Math.max(10, mapGlobal.view.getZoom() ?? 10),
        duration: 500,
      });
      inPanMode = true;
    }

    popupState.content = {
      name: boat.name,
      mmsi,
      speed: boat.speed,
      heading: boat.heading,
      cog: boat.cog,
      lat: boat.location[0],
      lng: boat.location[1],
      isMyBoat: false,
      host: boat.host,
      partId: boat.partId,
      isOnline: boat.isOnline ?? false,
    };
    popupState.visible = true;
    popupState.overlay?.setPosition(coords);

    onBoatPopupOpen?.(boat.partId || mmsi);
  }

  function formatCoord(val: number, isLat: boolean): string {
    const dir = isLat ? (val >= 0 ? "N" : "S") : val >= 0 ? "E" : "W";
    return Math.abs(val).toFixed(4) + "° " + dir;
  }

  // Closest Point of Approach. Flat-earth approx (good to ~1% within typical
  // AIS range) — projects positions to local north/east meters around own
  // boat, then solves for the time t that minimises |(P_tgt + V_tgt*t) -
  // (P_own + V_own*t)|. Returns null if relative velocity is ~0 (parallel
  // tracks, no closing) or if any input is missing/invalid.
  function computeCpa(
    ownLat: number,
    ownLng: number,
    ownCogDeg: number | null | undefined,
    ownSpdKn: number,
    tgtLat: number,
    tgtLng: number,
    tgtCogDeg: number | null | undefined,
    tgtSpdKn: number
  ): { cpaNm: number; tcpaMin: number } | null {
    if (ownCogDeg == null || tgtCogDeg == null) return null;
    if (!Number.isFinite(ownSpdKn) || !Number.isFinite(tgtSpdKn)) return null;
    const lat0 = (ownLat * Math.PI) / 180;
    const mPerDegLat = 111132.92 - 559.82 * Math.cos(2 * lat0);
    const mPerDegLng = 111412.84 * Math.cos(lat0);
    const dN = (tgtLat - ownLat) * mPerDegLat;
    const dE = (tgtLng - ownLng) * mPerDegLng;
    const knToMs = 0.514444;
    const ownVN = ownSpdKn * knToMs * Math.cos((ownCogDeg * Math.PI) / 180);
    const ownVE = ownSpdKn * knToMs * Math.sin((ownCogDeg * Math.PI) / 180);
    const tgtVN = tgtSpdKn * knToMs * Math.cos((tgtCogDeg * Math.PI) / 180);
    const tgtVE = tgtSpdKn * knToMs * Math.sin((tgtCogDeg * Math.PI) / 180);
    const dvN = tgtVN - ownVN;
    const dvE = tgtVE - ownVE;
    const dvSq = dvN * dvN + dvE * dvE;
    if (dvSq < 1e-6) return null; // no relative motion
    const tcpaSec = -(dN * dvN + dE * dvE) / dvSq;
    const futN = dN + dvN * tcpaSec;
    const futE = dE + dvE * tcpaSec;
    const cpaM = Math.sqrt(futN * futN + futE * futE);
    return { cpaNm: cpaM / 1852, tcpaMin: tcpaSec / 60 };
  }

  // Compass format: 14 -> "014°", null -> "—". Normalises into [0, 360).
  function compassFmt(deg: number | null | undefined): string {
    if (deg == null || !Number.isFinite(deg)) return "—";
    const norm = ((deg % 360) + 360) % 360;
    return Math.round(norm).toString().padStart(3, "0") + "°";
  }

  // True when the data panel has at least one row to show.
  let hasSensorData = $derived(
    sog != null || hdg != null || cog != null || depth != null
  );
  let hasDataPanel = $derived(
    hasSensorData || !!routeStats || !!cursorInfo || !!tideInfo || !!weatherInfo
  );

  // Sparkline geometry. Computed off the current tide series so the SVG
  // template stays a flat list of attributes (no inline {@const} math).
  // Recomputed on a 1-min tick to keep the "now" marker drifting.
  let sparkClock = $state(Date.now());
  $effect(() => {
    const id = setInterval(() => (sparkClock = Date.now()), 60 * 1000);
    return () => clearInterval(id);
  });
  const sparkW = 180;
  const sparkH = 44;
  // Live tide view: rebuilt every sparkClock tick from the raw 72h
  // series and hi/lo list so the "now" marker, the visible 24h window
  // (now-6h .. now+18h, keeping "now" at the 25% mark), the next
  // high/low predictions, and the current level all slide in real
  // time between 10-min refetches.
  let tideView = $derived.by(() => {
    if (!tideInfo) return null;
    const now = sparkClock;
    const winStart = now - 6 * 3600 * 1000;
    const winEnd = now + 18 * 3600 * 1000;
    const series = clipSeries(tideInfo.seriesAll, winStart, winEnd);
    const future = tideInfo.hiloAll.filter((p) => p.t.getTime() > now);
    const nextHigh = future.find((p) => p.type === "H") ?? null;
    const nextLow = future.find((p) => p.type === "L") ?? null;
    const currentLevel = interpCurrent(tideInfo.seriesAll, now);
    const windowedHilo = tideInfo.hiloAll.filter(
      (p) => p.t.getTime() >= winStart && p.t.getTime() <= winEnd
    );
    const allV = [...series.map((p) => p.v), ...windowedHilo.map((p) => p.v)];
    const seriesMin = allV.length > 0 ? Math.min(...allV) : 0;
    const seriesMax = allV.length > 0 ? Math.max(...allV) : 1;
    return {
      now,
      seriesStart: winStart,
      seriesEnd: winEnd,
      series,
      seriesMin,
      seriesMax,
      currentLevel,
      nextHigh,
      nextLow,
    };
  });

  let tideSpark = $derived.by(() => {
    if (!tideView || tideView.series.length < 2) return null;
    const pad = 3;
    const tStart = tideView.seriesStart;
    const tEnd = tideView.seriesEnd;
    const tRange = Math.max(tEnd - tStart, 1);
    const vMin = tideView.seriesMin;
    const vRange = Math.max(tideView.seriesMax - vMin, 0.01);
    const xOf = (t: number) => pad + ((sparkW - 2 * pad) * (t - tStart)) / tRange;
    const yOf = (v: number) => pad + (sparkH - 2 * pad) * (1 - (v - vMin) / vRange);
    const points = tideView.series
      .map((p) => `${xOf(p.t.getTime()).toFixed(1)},${yOf(p.v).toFixed(1)}`)
      .join(" ");
    const now = tideView.now;
    const inRange = now >= tStart && now <= tEnd;
    const nowX = inRange ? xOf(now) : null;
    const nowY =
      inRange && tideView.currentLevel !== null ? yOf(tideView.currentLevel) : null;
    return { points, nowX, nowY };
  });

  // ---- Tide data (NOAA Tides & Currents API, fetched directly from browser).
  // We download the full tide-prediction station list once per session
  // (cached in sessionStorage), find the nearest station to the boat's
  // current location, then fetch high/low predictions + current level.
  // Refetched every 10 min, or sooner if the boat moves enough to change
  // the rounded-key lat/lng (~6 nm).
  type TideStation = { id: string; name: string; lat: number; lng: number };
  type TidePoint = { tStr: string; t: Date; v: number; type: "H" | "L" };
  type TideSeriesPoint = { t: Date; v: number };
  // Raw tide data: station info + the unclipped 72h hourly series and
  // full hi/lo list NOAA returned. We keep these unclipped so the
  // visible window (and "now" marker) can slide with real time without
  // needing to refetch every minute. The displayed view — current
  // level, next high/low, the 24h window — is recomputed by tideView
  // on every sparkClock tick.
  let tideInfo = $state<{
    station: { id: string; name: string; distNm: number };
    hiloAll: TidePoint[];
    seriesAll: TideSeriesPoint[];
  } | null>(null);
  let tideStationCache: TideStation[] | null = null;
  let lastTideFetchKey = "";
  let tideRefetchTimer: ReturnType<typeof setInterval> | null = null;

  async function loadTideStations(): Promise<TideStation[]> {
    if (tideStationCache) return tideStationCache;
    try {
      const cached = sessionStorage.getItem("noaaTideStations");
      if (cached) {
        const parsed = JSON.parse(cached) as TideStation[];
        if (parsed && parsed.length > 0) {
          tideStationCache = parsed;
          return parsed;
        }
      }
    } catch {
      // ignore parse errors / storage disabled
    }
    const r = await fetch(
      "https://api.tidesandcurrents.noaa.gov/mdapi/prod/webapi/stations.json?type=tidepredictions"
    );
    if (!r.ok) throw new Error(`station list http ${r.status}`);
    const data = await r.json();
    const list: TideStation[] = (data.stations ?? []).map((s: any) => ({
      id: String(s.id),
      name: String(s.name),
      lat: Number(s.lat),
      lng: Number(s.lng),
    }));
    tideStationCache = list;
    try {
      sessionStorage.setItem("noaaTideStations", JSON.stringify(list));
    } catch {
      // sessionStorage may be full or disabled; in-memory cache is enough
    }
    return list;
  }

  function nearestTideStation(
    stations: TideStation[],
    lat: number,
    lng: number
  ): { station: TideStation; distNm: number } | null {
    let best: TideStation | null = null;
    let bestNm = Infinity;
    const R = 3440.065; // earth radius in nautical miles
    const lat1 = (lat * Math.PI) / 180;
    for (const s of stations) {
      const lat2 = (s.lat * Math.PI) / 180;
      const dLat = lat2 - lat1;
      const dLng = ((s.lng - lng) * Math.PI) / 180;
      const a =
        Math.sin(dLat / 2) ** 2 + Math.cos(lat1) * Math.cos(lat2) * Math.sin(dLng / 2) ** 2;
      const d = R * 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
      if (d < bestNm) {
        bestNm = d;
        best = s;
      }
    }
    return best ? { station: best, distNm: bestNm } : null;
  }

  // Format a JS Date as NOAA's "yyyyMMdd" using the browser's local
  // wall-clock time. With time_zone=lst_ldt NOAA interprets the date in
  // the station's local time; for nearby stations this matches.
  function fmtNoaaDate(d: Date): string {
    const y = d.getFullYear();
    const mo = String(d.getMonth() + 1).padStart(2, "0");
    const da = String(d.getDate()).padStart(2, "0");
    return `${y}${mo}${da}`;
  }

  async function fetchTidePredictions(stationId: string): Promise<{
    hilo: TidePoint[];
    series: TideSeriesPoint[];
  }> {
    const base = "https://api.tidesandcurrents.noaa.gov/api/prod/datagetter";
    const common = `application=viam-chartplotter&station=${stationId}&datum=MLLW&time_zone=lst_ldt&units=english&format=json`;
    // Fetch a 3-day window starting yesterday so we have plenty of points
    // both before and after "now". The sparkline window itself is set in
    // refreshTide and the polyline gets clipped to it there.
    const begin = fmtNoaaDate(new Date(Date.now() - 24 * 3600 * 1000));
    const [hiloRes, seriesRes] = await Promise.all([
      fetch(`${base}?product=predictions&interval=hilo&begin_date=${begin}&range=72&${common}`),
      fetch(`${base}?product=predictions&interval=h&begin_date=${begin}&range=72&${common}`),
    ]);
    if (!hiloRes.ok) throw new Error(`hilo http ${hiloRes.status}`);
    if (!seriesRes.ok) throw new Error(`series http ${seriesRes.status}`);
    const hiloData = await hiloRes.json();
    const seriesData = await seriesRes.json();
    // NOAA returns 200 with { error: { message } } for many "no data"
    // conditions (e.g., subordinate station + interval=h). Log and treat
    // as empty rather than throwing.
    if (hiloData?.error?.message) console.warn("tide hilo:", hiloData.error.message);
    if (seriesData?.error?.message) console.warn("tide series:", seriesData.error.message);
    const hilo: TidePoint[] = (hiloData.predictions ?? []).map((p: any) => ({
      tStr: p.t,
      t: new Date(p.t.replace(" ", "T")),
      v: parseFloat(p.v),
      type: p.type,
    }));
    const series: TideSeriesPoint[] = (seriesData.predictions ?? []).map((p: any) => ({
      t: new Date(p.t.replace(" ", "T")),
      v: parseFloat(p.v),
    }));
    return { hilo, series };
  }

  // Clip a series to [tStart, tEnd]. Linearly interpolates endpoints at
  // tStart and tEnd so the resulting polyline reaches both window edges
  // (rather than snapping to the nearest sample inside the window).
  function clipSeries(
    series: TideSeriesPoint[],
    tStart: number,
    tEnd: number
  ): TideSeriesPoint[] {
    if (series.length === 0) return [];
    const sorted = [...series].sort((a, b) => a.t.getTime() - b.t.getTime());
    const interpAt = (t: number): number | null => {
      if (sorted.length < 2) return null;
      if (t <= sorted[0].t.getTime()) return sorted[0].v;
      if (t >= sorted[sorted.length - 1].t.getTime()) return sorted[sorted.length - 1].v;
      for (let i = 1; i < sorted.length; i++) {
        const t1 = sorted[i - 1].t.getTime();
        const t2 = sorted[i].t.getTime();
        if (t >= t1 && t <= t2) {
          const f = (t - t1) / Math.max(t2 - t1, 1);
          return sorted[i - 1].v + f * (sorted[i].v - sorted[i - 1].v);
        }
      }
      return null;
    };
    const out: TideSeriesPoint[] = [];
    const startV = interpAt(tStart);
    if (startV !== null) out.push({ t: new Date(tStart), v: startV });
    for (const p of sorted) {
      const t = p.t.getTime();
      if (t > tStart && t < tEnd) out.push(p);
    }
    const endV = interpAt(tEnd);
    if (endV !== null) out.push({ t: new Date(tEnd), v: endV });
    return out;
  }

  // Linear interpolation of the hourly series at "now" for the current level.
  function interpCurrent(series: TideSeriesPoint[], now: number = Date.now()): number | null {
    if (series.length === 0) return null;
    if (series.length === 1) return series[0].v;
    if (now <= series[0].t.getTime()) return series[0].v;
    if (now >= series[series.length - 1].t.getTime()) return series[series.length - 1].v;
    for (let i = 1; i < series.length; i++) {
      const t1 = series[i - 1].t.getTime();
      const t2 = series[i].t.getTime();
      if (now >= t1 && now <= t2) {
        const f = (now - t1) / Math.max(t2 - t1, 1);
        return series[i - 1].v + f * (series[i].v - series[i - 1].v);
      }
    }
    return null;
  }

  // Build a synthetic tide series from hi/lo points using half-cosine
  // interpolation between adjacent peaks. Used when NOAA returns no hourly
  // data (subordinate stations only publish hi/lo). Samples every 15 min
  // across the window — dense enough for a smooth sparkline.
  function synthSeriesFromHilo(
    hilo: TidePoint[],
    windowStart: number,
    windowEnd: number
  ): TideSeriesPoint[] {
    if (hilo.length < 2) return [];
    const sorted = [...hilo].sort((a, b) => a.t.getTime() - b.t.getTime());
    const out: TideSeriesPoint[] = [];
    const stepMs = 15 * 60 * 1000;
    for (let t = windowStart; t <= windowEnd; t += stepMs) {
      // Find adjacent pair (p1, p2) such that p1.t <= t <= p2.t.
      let p1: TidePoint | null = null;
      let p2: TidePoint | null = null;
      for (let i = 0; i < sorted.length - 1; i++) {
        if (sorted[i].t.getTime() <= t && t <= sorted[i + 1].t.getTime()) {
          p1 = sorted[i];
          p2 = sorted[i + 1];
          break;
        }
      }
      if (!p1 || !p2) continue;
      const f = (t - p1.t.getTime()) / Math.max(p2.t.getTime() - p1.t.getTime(), 1);
      const mid = (p1.v + p2.v) / 2;
      const half = (p1.v - p2.v) / 2;
      out.push({ t: new Date(t), v: mid + half * Math.cos(Math.PI * f) });
    }
    return out;
  }

  async function refreshTide(lat: number, lng: number): Promise<void> {
    try {
      const stations = await loadTideStations();
      const nearest = nearestTideStation(stations, lat, lng);
      if (!nearest) {
        console.warn("tide: no nearest station found");
        return;
      }
      console.log(
        `tide: nearest station ${nearest.station.id} (${nearest.station.name}), ` +
          `${nearest.distNm.toFixed(1)} nm`
      );
      const { hilo, series: hourly } = await fetchTidePredictions(nearest.station.id);
      console.log(`tide: got ${hilo.length} hi/lo points, ${hourly.length} hourly points`);

      // Sparkline window is fixed at [now-6h, now+18h] so "now" always
      // sits at the 25% mark, regardless of NOAA's hourly grid alignment.
      // Store the raw 72h hi/lo + hourly series unclipped. The visible
      // window, current level, and next-high/low are derived in
      // tideView each sparkClock tick so they slide with real time
      // between 10-min refetches. If NOAA returned no hourly data
      // (subordinate station), synthesise the series from the hi/lo
      // peaks spanning the same window we'll be displaying.
      const now = Date.now();
      const seriesAll =
        hourly.length >= 2
          ? hourly
          : synthSeriesFromHilo(hilo, now - 24 * 3600 * 1000, now + 48 * 3600 * 1000);
      tideInfo = {
        station: {
          id: nearest.station.id,
          name: nearest.station.name,
          distNm: nearest.distNm,
        },
        hiloAll: hilo,
        seriesAll,
      };
    } catch (e) {
      console.warn("tide fetch failed", e);
    }
  }

  // Trigger refetch when location changes by ~6 nm (0.1° lat). Also kicks
  // a 10-minute background refresh so predictions stay current at anchor.
  $effect(() => {
    if (!myBoat?.location) return;
    const [lat, lng] = myBoat.location;
    if (lat === 0 && lng === 0) return;
    const key = `${Math.round(lat * 10) / 10},${Math.round(lng * 10) / 10}`;
    if (key === lastTideFetchKey) return;
    lastTideFetchKey = key;
    refreshTide(lat, lng);
    if (tideRefetchTimer) clearInterval(tideRefetchTimer);
    tideRefetchTimer = setInterval(
      () => {
        const loc = myBoat?.location;
        if (loc && !(loc[0] === 0 && loc[1] === 0)) {
          refreshTide(loc[0], loc[1]);
        }
      },
      10 * 60 * 1000
    );
    return () => {
      if (tideRefetchTimer) {
        clearInterval(tideRefetchTimer);
        tideRefetchTimer = null;
      }
    };
  });

  // Render "2024-01-15 14:23" -> "14:23" in the station's local time
  // (the t string is already in lst_ldt, so don't reparse to JS Date).
  function tideTimeFmt(p: TidePoint): string {
    return p.tStr.split(" ")[1]?.slice(0, 5) ?? p.tStr;
  }

  // ---- Weather (Open-Meteo current conditions). Owned by WeatherOverlays;
  // two-way bound here so the data-panel current-conditions row can render it.
  let weatherInfo = $state<WeatherInfo | null>(null);
  // Wind/wave sampler published by WeatherOverlays. The pointer handler calls
  // it (read at call time) so the wind/wave handles stay private to the child.
  let cursorSampler = $state<
    ((lon: number, lat: number) => CursorWeatherSample) | undefined
  >(undefined);

  function handleMapContainerClick(event: MouseEvent) {
    const target = event.target as HTMLElement;

    // Close boats panel if clicking outside of it
    if (boatsExpanded) {
      const boatsPanel = target.closest(".boats-panel");
      const boatsToggle = target.closest(".boats-toggle");

      if (!boatsPanel && !boatsToggle) {
        boatsExpanded = false;
      }
    }

    // Close layers panel if clicking outside of it
    if (layersExpanded) {
      const layersPanel = target.closest(".layer-controls");
      const layersToggle = target.closest(".layers-toggle");

      if (!layersPanel && !layersToggle) {
        layersExpanded = false;
      }
    }
  }

  onMount(() => {
    // setupMap is async (waits for the noaa-cache probe so layer URLs
    // resolve correctly regardless of bind port). Run the rendercomplete
    // and click handlers after the map is actually constructed.
    void (async () => {
      await setupMap();
      probeMyBoatIcon();

      // Listen for initial render complete to fade in map
      if (mapGlobal.map) {
        mapGlobal.map.once("rendercomplete", () => {
          mapLoaded = true;
        });
        // Fallback in case rendercomplete doesn't fire
        setTimeout(() => {
          mapLoaded = true;
        }, 1000);
      }
    })();

    // Add click-outside handler for panels
    const container = document.getElementById("map-container");
    if (container) {
      container.addEventListener("click", handleMapContainerClick as EventListener);
    }

    // Cleanup on unmount
    return () => {
      if (container) {
        container.removeEventListener("click", handleMapContainerClick as EventListener);
      }

      if (clearConfirmTimer !== undefined) {
        clearTimeout(clearConfirmTimer);
        clearConfirmTimer = undefined;
      }

      // Remove OpenLayers map event listeners to prevent memory leaks
      if (mapGlobal.map) {
        if (mapClickHandler) {
          mapGlobal.map.un("click", mapClickHandler);
        }
        if (mapPointerHandler) {
          mapGlobal.map.un("pointermove", mapPointerHandler);
        }
        if (mapPointerDragHandler) {
          mapGlobal.map.un("pointerdrag", mapPointerDragHandler);
        }
      }
    };
  });
</script>

<div
  id="map-container"
  class="relative {fullWidth ? 'lg:col-span-4 lg:row-span-6' : 'lg:col-span-3 lg:row-span-5'} row-span-3 border border-dark"
  class:layers-expanded={layersExpanded}
  class:boats-expanded={boatsExpanded}
  class:map-loaded={mapLoaded}
  class:full-width={fullWidth}
  class:chart-only={chartOnly}
>
  <div id="map" class="w-full aspect-video bg-white"></div>

  <WeatherOverlays
    {mapGlobal}
    {currentZoom}
    {myBoat}
    {api}
    {noaaCacheReachable}
    bind:cursorSampler
    bind:weatherInfo
  />

  <!-- Tiny "Powered By Viam" overlay anchored above the OL ScaleLine so it
       doesn't fight for the same bottom-left corner. Pointer-events off so
       it can't swallow map clicks. -->
  <img
    class="viam-logo-overlay"
    src={viamLogoUrl}
    alt="Powered by Viam"
    width="80"
    height="16"
  />

  <!-- Boat Info Popup -->
  <div id="boat-popup" class="boat-popup" class:hidden={!popupState.visible}>
    <button class="popup-closer" onclick={closePopup}>✕</button>
    <div class="popup-header">
      <h3 class="popup-title">
        {#if popupCountry}
          <span
            class="popup-flag"
            title={popupCountry[1]}
            aria-label={popupCountry[1]}>{flagEmoji(popupCountry[0])}</span
          >
        {/if}
        {popupState.content.name}
      </h3>
    </div>
    <div class="popup-columns" class:single-column={!popupState.content.isOnline}>
      {#if boatDetailSlot && popupState.content.host && popupState.content.partId}
        <div class="popup-detail-slot">
          {@render boatDetailSlot({
            host: popupState.content.host,
            partId: popupState.content.partId,
            name: popupState.content.name,
          })}
        </div>
      {/if}
      <div class="popup-content">
        <div class="popup-row">
          <span class="popup-label">SPD</span>
          <span class="popup-value">{popupState.content.speed.toFixed(1)} kn</span>
        </div>
        <div class="popup-row">
          <span class="popup-label">HDG</span>
          <span class="popup-value"
            >{popupState.content.heading.toFixed(0)}°<span
              class="compass-arrow"
              style="transform: rotate({popupState.content.heading}deg)">↑</span
            ></span
          >
        </div>
        {#if popupState.content.cog != null && Number.isFinite(popupState.content.cog)}
          <div class="popup-row">
            <span class="popup-label">COG</span>
            <span class="popup-value"
              >{popupState.content.cog.toFixed(0)}°<span
                class="compass-arrow"
                style="transform: rotate({popupState.content.cog}deg)">↑</span
              ></span
            >
          </div>
        {/if}
        <div class="popup-row">
          <span class="popup-label">LAT</span>
          <span class="popup-value">{formatCoord(popupState.content.lat, true)}</span>
        </div>
        <div class="popup-row">
          <span class="popup-label">LNG</span>
          <span class="popup-value">{formatCoord(popupState.content.lng, false)}</span>
        </div>
        {#if popupState.content.length != null}
          <div class="popup-row">
            <span class="popup-label">LEN</span>
            <span class="popup-value">{popupState.content.length.toFixed(0)} m</span>
          </div>
        {/if}
        {#if popupState.content.destination}
          <div class="popup-row">
            <span class="popup-label">DST</span>
            <span class="popup-value">{popupState.content.destination}</span>
          </div>
        {/if}
        {#if popupState.content.cpaNm !== null && popupState.content.tcpaMin !== null && popupState.content.tcpaMin >= 0}
          <div class="popup-row">
            <span class="popup-label">CPA</span>
            <span class="popup-value"
              >{popupState.content.cpaNm.toFixed(2)} nm in {popupState.content.tcpaMin < 1
                ? "<1"
                : popupState.content.tcpaMin.toFixed(0)} min</span
            >
          </div>
        {/if}
      </div>
    </div>
    <div class="popup-arrow"></div>
  </div>

  <!-- Depth Tooltip -->
  <div id="depth-tooltip" class="depth-tooltip"></div>

  <!-- Navaid Hover Tooltip -->
  <div id="navaid-tooltip" class="navaid-tooltip"></div>

  <!-- My-boat track-time Tooltip -->
  <div id="track-time-tooltip" class="track-time-tooltip"></div>

  <!-- AIS flag/country hover tooltip -->
  <div id="ais-tooltip" class="ais-tooltip"></div>


  <!-- Tile-URL popup: shown when "Tile URL" mode is on and the user clicks
       the map. Plain absolutely-positioned div in the centre top, simple
       CSS — no OL overlay needed since the content isn't bound to a coord. -->
  <div id="tile-url-popup" class="tile-url-popup"></div>

  <!-- Edit popup. Shown when the user clicks a waypoint or a route segment
       in edit mode. The element must always exist for OL's Overlay to bind
       to; visibility is driven by class="hidden". -->
  <div
    id="edit-waypoint-popup"
    class="edit-waypoint-popup"
    class:hidden={!editPopupState.visible}
    class:delete={editPopupState.mode === "delete"}
  >
    <button
      class="edit-waypoint-btn"
      class:delete={editPopupState.mode === "delete"}
      onclick={confirmEditPopup}
    >
      {#if editPopupState.mode === "delete"}
        Delete this waypoint
      {:else}
        + Add waypoint here
      {/if}
    </button>
    <button class="edit-waypoint-close" onclick={closeEditPopup} aria-label="Cancel">✕</button>
  </div>

  {#if inPanMode}
    <button class="stop-panning-btn" onclick={stopPanning}>Stop Panning</button>
  {/if}

  <div class="layer-controls">
    <!--
      Layers are split into two groups:
        1. Base maps (open street map / noaa / checkmate / noaa-ecdis)
           and their children — mutually exclusive radio buttons. Picking
           one auto-disables the others so we never paint two raster
           bases on top of each other.
        2. Overlays (boat / ais / airstream) and their children —
           independent checkboxes that ride on top of whichever base
           map is selected.
      The two groups are separated by a horizontal divider.
      BASE_LAYER_NAMES / isBaseLayerGroup are defined in the script.
    -->
    {#each mapGlobal.layerOptions as l, idx}
      {@const parentLayer = l.parent
        ? mapGlobal.layerOptions.find((p) => p.name === l.parent)
        : null}
      {@const isParentOff = parentLayer && !parentLayer.on}
      {@const isHidden = l.name === "airstream" && !airstreamConfigured}
      {@const isBaseLayer = BASE_LAYER_NAMES.includes(l.name)}
      {#if !isHidden && isBaseLayerGroup(l)}
      <label class:child-layer={l.parent} class:disabled={isParentOff}>
        <input
          type={isBaseLayer ? "radio" : "checkbox"}
          name={isBaseLayer ? "base-layer" : undefined}
          checked={mapGlobal.layerOptions[idx].on}
          onchange={(e) => {
            const checked = (e.currentTarget as HTMLInputElement).checked;
            mapGlobal.layerOptions[idx].on = checked;
            // Radio behaviour for base layers: turning one on flips
            // every other base layer off so we never have two
            // simultaneously selected.
            if (isBaseLayer && checked) {
              for (var other of mapGlobal.layerOptions) {
                if (other.name !== l.name && BASE_LAYER_NAMES.includes(other.name)) {
                  other.on = false;
                }
              }
            }
            saveLayerStates();
          }}
          disabled={isParentOff}
        />
        {l.displayName || l.name}
      </label>
      {/if}
    {/each}

    <hr class="layer-divider" />

    {#each mapGlobal.layerOptions as l, idx}
      {@const parentLayer = l.parent
        ? mapGlobal.layerOptions.find((p) => p.name === l.parent)
        : null}
      {@const isParentOff = parentLayer && !parentLayer.on}
      {@const isHidden = l.name === "airstream" && !airstreamConfigured}
      {#if !isHidden && !isBaseLayerGroup(l)}
      {#if l.name === "weather"}
        <!-- Folder-style section header: no checkbox, just labels the
             wind/waves rows that follow as a group. -->
        <div class="layer-section-header">{l.displayName || l.name}</div>
      {:else}
      <label class:child-layer={l.parent} class:disabled={isParentOff}>
        <input
          type="checkbox"
          checked={mapGlobal.layerOptions[idx].on}
          onchange={(e) => {
            const checked = (e.currentTarget as HTMLInputElement).checked;
            mapGlobal.layerOptions[idx].on = checked;
            saveLayerStates();
            if (l.name === "airstream") {
              if (checked) {
                lastAirstreamBboxKey = "";
                maybeEmitAirstreamBbox();
              } else if (onAirstreamBboxChange) {
                lastAirstreamBboxKey = "";
                onAirstreamBboxChange(null);
              }
            }
          }}
          disabled={isParentOff}
        />
        {l.displayName || l.name}
        {#if l.name === "heading-line"}
          <select
            class="heading-line-length"
            value={headingLineLengthNm}
            onchange={(e) => setHeadingLineLength(Number(e.currentTarget.value))}
            disabled={isParentOff || !l.on}
            onclick={(e) => e.stopPropagation()}
            aria-label="heading line length"
          >
            {#each HEADING_LINE_LENGTH_OPTIONS as nm}
              <option value={nm}>{nm} nm</option>
            {/each}
          </select>
        {:else if l.name === "ais-projection"}
          <select
            class="heading-line-length"
            value={aisProjectionMinutes}
            onchange={(e) => setAisProjectionMinutes(Number(e.currentTarget.value))}
            disabled={isParentOff || !l.on}
            onclick={(e) => e.stopPropagation()}
            aria-label="ais projection length in minutes"
          >
            {#each AIS_PROJECTION_OPTIONS as min}
              <option value={min}>{min} min</option>
            {/each}
          </select>
        {/if}
      </label>
      {/if}
      {/if}
    {/each}

    {#if depthSensorAvailable}
      <hr class="layer-divider" />
      <!-- Track-rendering option (not a layer of its own). Lives outside
           the each loop because it's a style toggle for the existing
           "track" layer rather than a toggleable layer. The legend
           gradient mirrors the colour ramp drawn on the track itself
           so the user can map colour to depth at a glance. -->
      <label class="depth-color-toggle">
        <input
          type="checkbox"
          checked={depthColorTrack}
          onchange={(e) => (depthColorTrack = (e.currentTarget as HTMLInputElement).checked)}
        />
        color track by depth
        <span class="depth-color-legend"></span>
        <span class="depth-color-legend-label">0–10 ft</span>
      </label>
    {/if}
  </div>

  <div class="bottom-controls">
    {#if enableBoatsPanel}
      <!-- Boats panel anchors to the boats-toggle button in .left-toolbar -->
      <div class="boats-panel">
        <div class="boats-controls">
          <button class="select-btn" onclick={selectAllBoats} title="Select all boats"
            >Select all</button
          >
          <button class="select-btn" onclick={deselectAllBoats} title="Deselect all boats"
            >Deselect all</button
          >
        </div>
        <div class="boats-list">
          {#if myBoat}
            <label class="boat-item">
              <input
                type="checkbox"
                checked={visibleBoats.has("myBoat")}
                onchange={() => toggleBoatVisibility("myBoat")}
              />
              <span class="boat-name my-boat">My Boat</span>
            </label>
          {/if}
          {#if boats}
            {@const searchLower = boatSearchTerm.toLowerCase()}
            {@const onlineBoats = boats.filter(
              (b) =>
                b.mmsi &&
                b.isOnline !== false &&
                (!boatSearchTerm.trim() ||
                  b.name.toLowerCase().includes(searchLower) ||
                  b.mmsi?.toLowerCase().includes(searchLower))
            )}
            {@const offlineBoats = boats.filter(
              (b) =>
                b.mmsi &&
                b.isOnline === false &&
                (!boatSearchTerm.trim() ||
                  b.name.toLowerCase().includes(searchLower) ||
                  b.mmsi?.toLowerCase().includes(searchLower))
            )}
            {#each onlineBoats as boat}
              <label class="boat-item">
                <input
                  type="checkbox"
                  checked={visibleBoats.has(boat.mmsi!)}
                  onchange={() => toggleBoatVisibility(boat.mmsi!)}
                />
                <span class="boat-name">{boat.name}</span>
              </label>
            {/each}
            {#if showOfflineBoatsInPanel && offlineBoats.length > 0}
              <div class="boats-separator">Offline boats:</div>
              {#each offlineBoats as boat}
                <label class="boat-item offline">
                  <input
                    type="checkbox"
                    checked={visibleBoats.has(boat.mmsi!)}
                    onchange={() => toggleBoatVisibility(boat.mmsi!)}
                  />
                  <span class="boat-name">{boat.name}</span>
                </label>
              {/each}
            {/if}
          {/if}
        </div>
        <input
          type="text"
          class="boat-search-input"
          placeholder="Search boats..."
          bind:value={boatSearchTerm}
        />
        <button class="fit-all-btn" onclick={fitToVisibleBoats}> Fit All Visible </button>
      </div>

    {/if}
  </div>

  <!-- Left-side toolbar: every map control stacks here under the OL
       zoom +/- buttons so the toolbar reads top-to-bottom in one place
       instead of being scattered across the corners. Buttons are flex
       children — conditional ones (add waypoint, clear waypoints, boats)
       appear and disappear without the others jumping around. -->
  <div class="left-toolbar">
    <button
      class="layers-toggle"
      class:active={layersExpanded}
      onclick={() => (layersExpanded = !layersExpanded)}
      data-tip="Layers"
      aria-label="Toggle map layers"
      aria-pressed={layersExpanded}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="15"
        height="15"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
        stroke-linejoin="round"
        aria-hidden="true"
      >
        <path d="m12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83Z" />
        <path d="m22 17.65-9.17 4.16a2 2 0 0 1-1.66 0L2 17.65" />
        <path d="m22 12.65-9.17 4.16a2 2 0 0 1-1.66 0L2 12.65" />
      </svg>
    </button>

    {#if enableBoatsPanel}
      <button
        class="boats-toggle"
        class:active={boatsExpanded}
        onclick={() => (boatsExpanded = !boatsExpanded)}
        data-tip="Boats"
        aria-label="Toggle boats panel"
        aria-pressed={boatsExpanded}
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="15"
          height="15"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          aria-hidden="true"
        >
          <path d="M2 21c.6.5 1.2 1 2.5 1 2.5 0 2.5-2 5-2 1.3 0 1.9.5 2.5 1s1.2 1 2.5 1c2.5 0 2.5-2 5-2 1.3 0 1.9.5 2.5 1" />
          <path d="M19.38 20A11.6 11.6 0 0 0 21 14l-9-4-9 4c0 2.9.94 5.34 2.81 7.76" />
          <path d="M19 13V7a2 2 0 0 0-2-2H7a2 2 0 0 0-2 2v6" />
          <path d="M12 10v4" />
          <path d="M12 2v3" />
        </svg>
      </button>
    {/if}

    <button
      class="tile-url-toggle"
      class:active={tileUrlActive}
      onclick={() => (tileUrlActive = !tileUrlActive)}
      data-tip="When on, click the map to copy the checkmate tile URL for that location"
      aria-label="Tile URL debug mode"
    >
      {"{}"}
    </button>

    <button
    class="measure-toggle"
    class:active={measureActive}
    onclick={toggleMeasure}
    aria-pressed={measureActive}
    data-tip="Measure distance"
    aria-label="Measure distance"
  >
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
      ><path
        d="M21.3 15.3a2.4 2.4 0 0 1 0 3.4l-2.6 2.6a2.4 2.4 0 0 1-3.4 0L2.7 8.7a2.41 2.41 0 0 1 0-3.4l2.6-2.6a2.41 2.41 0 0 1 3.4 0Z"
      /><path d="m14.5 12.5 2-2" /><path d="m11.5 9.5 2-2" /><path d="m8.5 6.5 2-2" /><path
        d="m17.5 15.5 2-2"
      /></svg
    >
  </button>

  {#if onAddWaypoint}
    <!-- Horizontal pair: pin (add) + ✕ (clear). Clear only renders when
         a route exists, so the pin is alone otherwise. The wrapper keeps
         them on the same row inside the otherwise-vertical toolbar. -->
    <div class="toolbar-row">
      <button
        class="add-waypoint-toggle"
        class:active={addWaypointActive}
        onclick={toggleAddWaypoint}
        aria-pressed={addWaypointActive}
        data-tip={addWaypointActive
          ? "Click on the chart to add a waypoint (active)"
          : "Add a route waypoint from current position"}
        aria-label="Add waypoint"
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="15"
          height="15"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
          aria-hidden="true"
        >
          <path d="M12 21s-7-7.5-7-12a7 7 0 0 1 14 0c0 4.5-7 12-7 12Z" />
          <circle cx="12" cy="9" r="2.5" />
        </svg>
      </button>
      {#if addWaypointActive && navWaypoints && navWaypoints.length > 0}
        <button
          class="clear-waypoints-btn"
          class:armed={clearConfirmArmed}
          onclick={clearWaypoints}
          data-tip={clearConfirmArmed
            ? "Click again to confirm clearing all waypoints"
            : "Clear all route waypoints"}
          aria-label={clearConfirmArmed ? "Confirm clear route" : "Clear route"}
        >
          {clearConfirmArmed ? "?" : "✕"}
        </button>
      {/if}
    </div>
  {/if}

  <button
    class="heads-up-toggle"
    class:active={headsUpActive}
    onclick={toggleHeadsUp}
    aria-pressed={headsUpActive}
    disabled={!myBoat}
    data-tip={headsUpActive ? "Heads-up orientation (on)" : "Heads-up orientation (north up)"}
    aria-label="Toggle heads-up orientation"
  >
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
      ><circle cx="12" cy="12" r="10" /><polygon
        points="16.24 7.76 14.12 14.12 7.76 16.24 9.88 9.88 16.24 7.76"
      /></svg
    >
  </button>

  <button
    class="boat-position-toggle"
    class:active={boatPositionMode === "bottom"}
    onclick={toggleBoatPosition}
    aria-pressed={boatPositionMode === "bottom"}
    disabled={!myBoat}
    data-tip={boatPositionMode === "bottom"
      ? "Boat position: bottom 20% (click for centered)"
      : "Boat position: centered (click for bottom 20%)"}
    aria-label="Toggle boat position on screen"
  >
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
    >
      <rect x="3" y="3" width="18" height="18" rx="2" />
      {#if boatPositionMode === "bottom"}
        <circle cx="12" cy="18" r="2" fill="currentColor" />
      {:else}
        <circle cx="12" cy="12" r="2" fill="currentColor" />
      {/if}
    </svg>
  </button>

  <button
    class="auto-zoom-toggle"
    class:active={autoZoomActive}
    onclick={toggleAutoZoom}
    aria-pressed={autoZoomActive}
    data-tip={autoZoomActive
      ? "Auto-zoom (on): zoom follows boat speed"
      : "Auto-zoom (off): manual zoom"}
    aria-label="Toggle auto-zoom"
  >
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
    >
      <circle cx="11" cy="11" r="7" />
      <line x1="21" y1="21" x2="16.65" y2="16.65" />
      <line x1="8" y1="11" x2="14" y2="11" />
      <line x1="11" y1="8" x2="11" y2="14" />
    </svg>
  </button>
  </div>

  {#if measureActive && measureDistance !== null}
    <div class="measure-result">
      {measureDistance.toFixed(2)} nm ({(measureDistance * 1.15078).toFixed(2)} mi)
    </div>
  {/if}

  <!-- Combined data panel: sensors (SOG/HDG/COG/Depth), nav (route stats),
       and cursor (lat/lng + bearing/distance from boat). Pinned to top-right
       of the map below the toolbar. Sections are separated by dividers and
       only render when their data is present. -->
  {#if hasDataPanel}
    <div class="data-panel" class:edit={addWaypointActive}>
      {#if hasSensorData}
        <div class="data-panel-section">
          {#if sog != null}
            <div class="data-panel-row">
              <span class="data-panel-label">SOG</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{sog.toFixed(2)}</span><sup>kn</sup>
              </span>
            </div>
          {/if}
          {#if hdg != null || cog != null}
            <div class="data-panel-row">
              <span class="data-panel-label">HDG/COG</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{compassFmt(hdg)}</span> /
                <span class="data-panel-bold">{compassFmt(cog)}</span>
              </span>
            </div>
          {/if}
          {#if depth != null}
            <div class="data-panel-row">
              <span class="data-panel-label">Depth</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{depth.toFixed(1)}</span><sup>ft</sup>
              </span>
            </div>
          {/if}
        </div>
      {/if}
      {#if routeStats}
        <div class="data-panel-section data-panel-nav">
          <div class="data-panel-row">
            <span class="data-panel-label">Next</span>
            <span class="data-panel-value">
              <span class="data-panel-bold">{routeStats.next.distNm.toFixed(2)}</span><sup>nm</sup>
              · {routeStats.next.headingDeg.toFixed(0)}°
              · {formatDurationMin(routeStats.next.minutes)}
              · ETA {formatEta(routeStats.next.minutes)}
            </span>
          </div>
          {#if routeStats.final.waypointCount > 1}
            <div class="data-panel-row">
              <span class="data-panel-label">Final</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{routeStats.final.distNm.toFixed(2)}</span><sup>nm</sup>
                · {formatDurationMin(routeStats.final.minutes)}
                · ETA {formatEta(routeStats.final.minutes)}
              </span>
            </div>
          {/if}
          {#if addWaypointActive}
            <div class="data-panel-hint">click to add · drag waypoints to move</div>
          {/if}
        </div>
      {/if}
      {#if tideInfo}
        <a
          class="data-panel-section data-panel-tide data-panel-link"
          href={`https://tidesandcurrents.noaa.gov/stationhome.html?id=${tideInfo.station.id}`}
          target="_blank"
          rel="noopener noreferrer"
          title="Open NOAA tide station page"
        >
          <div class="data-panel-stack">
            <span class="data-panel-label">Tide</span>
            <span class="data-panel-station">{tideInfo.station.name}</span>
            <span class="data-panel-station">{tideInfo.station.distNm.toFixed(1)} nm away</span>
          </div>
          {#if tideSpark}
            <svg
              class="tide-spark"
              viewBox="0 0 {sparkW} {sparkH}"
              preserveAspectRatio="none"
            >
              <polyline
                points={tideSpark.points}
                fill="none"
                stroke="#4ade80"
                stroke-width="1.5"
              />
              {#if tideSpark.nowX !== null}
                <line
                  x1={tideSpark.nowX}
                  y1="0"
                  x2={tideSpark.nowX}
                  y2={sparkH}
                  stroke="#fff"
                  stroke-width="1"
                  stroke-dasharray="2,2"
                  opacity="0.7"
                />
                {#if tideSpark.nowY !== null}
                  <circle cx={tideSpark.nowX} cy={tideSpark.nowY} r="2.5" fill="#fff" />
                {/if}
              {/if}
            </svg>
          {/if}
          {#if tideView && tideView.currentLevel !== null}
            <div class="data-panel-row">
              <span class="data-panel-label">Now</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{tideView.currentLevel.toFixed(2)}</span><sup>ft</sup>
              </span>
            </div>
          {/if}
          {#if tideView?.nextHigh}
            <div class="data-panel-row">
              <span class="data-panel-label">High</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{tideView.nextHigh.v.toFixed(2)}</span><sup>ft</sup>
                · {tideTimeFmt(tideView.nextHigh)}
              </span>
            </div>
          {/if}
          {#if tideView?.nextLow}
            <div class="data-panel-row">
              <span class="data-panel-label">Low</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{tideView.nextLow.v.toFixed(2)}</span><sup>ft</sup>
                · {tideTimeFmt(tideView.nextLow)}
              </span>
            </div>
          {/if}
        </a>
      {/if}
      {#if weatherInfo && myBoat?.location}
        <a
          class="data-panel-section data-panel-weather data-panel-link"
          href={`https://www.windy.com/?${myBoat.location[0].toFixed(4)},${myBoat.location[1].toFixed(4)},10`}
          target="_blank"
          rel="noopener noreferrer"
          title="Open Windy.com forecast"
        >
          <div class="data-panel-stack">
            <span class="data-panel-label">Wx · next 4h</span>
          </div>
          {#if weatherInfo.tempNowF !== null}
            <div class="data-panel-row">
              <span class="data-panel-label">Temp</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{weatherInfo.tempNowF.toFixed(0)}</span><sup>°F</sup>
                · {weatherInfo.tempMinF.toFixed(0)}–{weatherInfo.tempMaxF.toFixed(0)}°
              </span>
            </div>
          {/if}
          {#if weatherInfo.windNowKn !== null}
            <div class="data-panel-row">
              <span class="data-panel-label">Wind</span>
              <span class="data-panel-value">
                <span class="data-panel-bold">{weatherInfo.windNowKn.toFixed(0)}</span><sup>kn</sup>
                {#if weatherInfo.windNowDirDeg !== null}
                  · {compassFmt(weatherInfo.windNowDirDeg)}
                {/if}
                · max {weatherInfo.windMaxKn.toFixed(0)}
              </span>
            </div>
          {/if}
          <div class="data-panel-row">
            <span class="data-panel-label">Rain</span>
            <span class="data-panel-value">
              {#if weatherInfo.rainTotalIn > 0}
                <span class="data-panel-bold">{weatherInfo.rainTotalIn.toFixed(2)}</span><sup>in</sup>
                · {weatherInfo.rainHoursAny}h
              {:else}
                <span class="data-panel-bold">none</span>
              {/if}
            </span>
          </div>
          {#if weatherInfo.sunriseLocal && weatherInfo.sunsetLocal}
            <div class="data-panel-row">
              <span class="data-panel-label">Sun</span>
              <span class="data-panel-value">
                ↑ {weatherInfo.sunriseLocal} · ↓ {weatherInfo.sunsetLocal}
              </span>
            </div>
          {/if}
        </a>
      {/if}
      {#if cursorInfo}
        <div class="data-panel-section data-panel-cursor">
          <div class="data-panel-stack">
            <span class="data-panel-label">Cursor</span>
            <span class="data-panel-value">{formatCoord(cursorInfo.lat, true)}</span>
            <span class="data-panel-value">{formatCoord(cursorInfo.lng, false)}</span>
            {#if cursorInfo.nm !== null && cursorInfo.brg !== null}
              <span class="data-panel-value">
                <span class="data-panel-bold">{cursorInfo.nm.toFixed(2)}</span><sup>nm</sup>
                @ {cursorInfo.brg.toFixed(0).padStart(3, "0")}°
              </span>
            {/if}
            {#if cursorInfo.windKt !== null && cursorInfo.windFromDeg !== null}
              {@const windColor = colorForValue(
                WIND_COLOR_SCALE,
                cursorInfo.windKt / MS_TO_KT,
                15,
              )}
              <span class="data-panel-value" style="color: {windColor}">
                <span class="weather-swatch" style="background: {windColor}"
                ></span>
                wind <span class="data-panel-bold">{cursorInfo.windKt.toFixed(0)}</span><sup>kt</sup>
                from {cursorInfo.windFromDeg.toFixed(0).padStart(3, "0")}°
              </span>
            {/if}
            {#if cursorInfo.waveM !== null && cursorInfo.waveFromDeg !== null}
              {@const waveColor = colorForValue(
                WAVE_COLOR_SCALE,
                cursorInfo.waveM,
                WAVE_RANGE_MAX_M,
              )}
              <span class="data-panel-value" style="color: {waveColor}">
                <span class="weather-swatch" style="background: {waveColor}"
                ></span>
                wave <span class="data-panel-bold">{(cursorInfo.waveM * METERS_TO_FEET).toFixed(1)}</span><sup>ft</sup>
                from {cursorInfo.waveFromDeg.toFixed(0).padStart(3, "0")}°
              </span>
            {/if}
          </div>
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .depth-tooltip {
    background: rgba(0, 0, 0, 0.8);
    color: white;
    padding: 2px 6px;
    border-radius: 3px;
    font-size: 12px;
    white-space: nowrap;
    pointer-events: none;
  }

  .track-time-tooltip {
    background: rgba(0, 0, 0, 0.8);
    color: white;
    padding: 2px 6px;
    border-radius: 3px;
    font-size: 12px;
    white-space: nowrap;
    pointer-events: none;
  }
  .track-time-tooltip:empty {
    display: none;
  }

  .ais-tooltip {
    background: rgba(0, 0, 0, 0.85);
    color: white;
    padding: 3px 8px;
    border-radius: 3px;
    font-size: 13px;
    white-space: nowrap;
    pointer-events: none;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.3);
  }
  .ais-tooltip:empty {
    display: none;
  }

  .navaid-tooltip {
    background: rgba(0, 0, 0, 0.85);
    color: white;
    padding: 6px 10px;
    border: 1px solid #6b7280;
    border-radius: 4px;
    font-size: 12px;
    line-height: 1.3;
    pointer-events: none;
    max-width: 280px;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.3);
  }
  .navaid-tooltip:empty {
    display: none;
  }
  .navaid-tooltip :global(.navaid-tt-title) {
    font-weight: 600;
    color: #fde68a;
    margin-bottom: 2px;
  }
  .navaid-tooltip :global(.navaid-tt-sub) {
    color: rgba(255, 255, 255, 0.6);
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    margin-bottom: 4px;
  }
  .navaid-tooltip :global(.navaid-tt-row) {
    font-variant-numeric: tabular-nums;
  }
  .navaid-tooltip :global(.navaid-tt-info) {
    margin-top: 4px;
    color: rgba(255, 255, 255, 0.75);
    font-style: italic;
    font-size: 11px;
    white-space: normal;
  }

  /* Combined data panel: SOG/HDG/COG/Depth, route stats, and cursor info.
     Pinned to top-right of the map, below the toolbar row. Sections are
     separated by horizontal dividers. pointer-events:none so it never
     blocks click-through to the map. */
  .data-panel {
    position: absolute;
    top: 50px;
    right: 10px;
    z-index: 1002;
    background: rgba(0, 0, 0, 0.7);
    color: white;
    border: 1px solid #6b7280;
    border-radius: 4px;
    font-size: 16px;
    line-height: 1.3;
    white-space: nowrap;
    pointer-events: none;
    font-variant-numeric: tabular-nums;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.3);
    max-width: calc(100% - 20px);
  }
  .data-panel-section {
    padding: 8px 12px;
  }
  .data-panel-section + .data-panel-section {
    border-top: 1px solid #6b7280;
  }
  .data-panel-row {
    display: flex;
    gap: 10px;
    align-items: baseline;
    justify-content: space-between;
  }
  .data-panel-label {
    color: #9ca3af;
    font-size: 0.85em;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .data-panel-value {
    text-align: right;
  }
  .data-panel-bold {
    font-weight: 700;
  }
  /* Small colour chip placed before a wind/wave readout in the cursor
     popup so the displayed magnitude is visually tied to the gradient
     legend bottom-left. Border keeps it visible on both light and
     dark panel backgrounds. */
  .weather-swatch {
    display: inline-block;
    width: 10px;
    height: 10px;
    margin-right: 4px;
    border: 1px solid rgba(255, 255, 255, 0.4);
    border-radius: 2px;
    vertical-align: 0px;
  }
  .data-panel sup {
    font-size: 0.7em;
    margin-left: 1px;
    color: #d1d5db;
  }
  .data-panel-nav {
    color: #fde68a;
  }
  .data-panel-nav .data-panel-label {
    color: #f59e0b;
  }
  .data-panel-cursor {
    color: #bae6fd;
  }
  .data-panel-cursor .data-panel-label {
    color: #7dd3fc;
  }
  .data-panel-tide {
    color: #bbf7d0;
  }
  .data-panel-tide .data-panel-label {
    color: #4ade80;
  }
  .data-panel-weather {
    color: #fde68a;
  }
  .data-panel-weather .data-panel-label {
    color: #facc15;
  }
  /* Re-enable clicks on the linkable sections (parent .data-panel disables
     pointer events so the chart stays draggable underneath). */
  .data-panel-link {
    pointer-events: auto;
    cursor: pointer;
    text-decoration: none;
    display: block;
  }
  .data-panel-link:hover {
    background: rgba(255, 255, 255, 0.05);
  }
  /* Stacked label-above-value group, used by Cursor and Tide sections to
     keep the panel narrow. */
  .data-panel-stack {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 1px;
  }
  .data-panel-station {
    font-size: 0.78em;
    opacity: 0.85;
    line-height: 1.1;
  }
  .tide-spark {
    display: block;
    margin-top: 6px;
    width: 180px;
    height: 44px;
  }
  .data-panel.edit {
    border-color: #f59e0b;
  }
  .data-panel-hint {
    margin-top: 4px;
    color: #fef3c7;
    font-size: 0.75em;
    opacity: 0.85;
  }

  .tile-url-popup {
    display: none;
    position: absolute;
    top: 12px;
    left: 50%;
    transform: translateX(-50%);
    background: rgba(15, 23, 42, 0.95);
    color: white;
    padding: 8px 12px;
    border-radius: 4px;
    font-size: 12px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    z-index: 1000;
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
  }

  .tile-url-popup :global(a) {
    color: #93c5fd;
    text-decoration: none;
  }

  .tile-url-popup :global(a:hover) {
    text-decoration: underline;
  }

  /* Tiny Viam logo superimposed on the map bottom-left. Sits above OL's
     ScaleLine (which defaults to bottom-left ~18 px tall, ~8 px inset)
     and below toolbar tooltips. White invert + reduced opacity so the
     dark wordmark reads against the chart without dominating it. */
  .viam-logo-overlay {
    position: absolute;
    /* Bottom-left, lowest layer of the stacked corner controls
       (logo → distance scale → wave legend, reading bottom to top). */
    bottom: 6px;
    left: 8px;
    z-index: 1000;
    opacity: 0.7;
    filter: invert(1) drop-shadow(0 0 2px rgba(0, 0, 0, 0.6));
    pointer-events: none;
    user-select: none;
  }

  /* Push OL's bottom-left distance scale up above the Viam wordmark.
     `bar: true` renders as `.ol-scale-bar`, NOT `.ol-scale-line` (text
     mode), so we target both for safety. 50 px gives the ~28 px-tall
     bar plenty of clearance over the logo at 6 px. */
  :global(.ol-scale-line),
  :global(.ol-scale-bar) {
    bottom: 50px !important;
    left: 8px !important;
  }

  /* Left-side toolbar: vertical stack of map controls anchored just
     below OpenLayers' built-in zoom +/- buttons (which sit at top:5,
     left:5 and run ~75px tall). Children are flex items so conditional
     buttons (add waypoint, clear waypoints, boats) can appear/disappear
     without breaking the layout. */
  /* Chart-extended (kiosk) mode: no boat/robot. Keep the layers button (the map
     selector — its panel also holds the weather toggles) and the tile-url debug
     box (the {} toggle, useful for grabbing tile URLs in kiosk deployments), but
     hide the boat/nav controls and the boat data panel. (The add-waypoint and
     boats buttons aren't rendered in this mode anyway — no nav service / boats
     panel.) */
  .chart-only .data-panel,
  .chart-only .boats-toggle,
  .chart-only .measure-toggle {
    display: none;
  }
  .left-toolbar {
    position: absolute;
    top: 90px;
    left: 8px;
    display: flex;
    flex-direction: column;
    gap: 5px;
    z-index: 1001;
  }

  /* Sub-row for paired buttons inside the otherwise-vertical toolbar.
     Used for add-waypoint (pin) + clear-waypoints (✕) so the cancel
     button sits to the right of the add button instead of below it. */
  .toolbar-row {
    display: flex;
    flex-direction: row;
    gap: 5px;
  }

  /* Custom tooltips: show instantly to the right of any toolbar button
     that has a data-tip attribute. Native `title` has a ~700 ms delay
     and inconsistent styling, so we use data-tip + pseudo-element
     instead. aria-label still drives screen readers. */
  .left-toolbar button {
    position: relative;
  }
  .left-toolbar button[data-tip]:hover::after,
  .left-toolbar button[data-tip]:focus-visible::after {
    content: attr(data-tip);
    position: absolute;
    top: 50%;
    left: calc(100% + 8px);
    transform: translateY(-50%);
    background: rgba(0, 0, 0, 0.85);
    color: white;
    padding: 4px 8px;
    border-radius: 4px;
    font-size: 12px;
    line-height: 1.3;
    white-space: nowrap;
    pointer-events: none;
    z-index: 1100;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.3);
  }
  .left-toolbar button[data-tip]:hover::before,
  .left-toolbar button[data-tip]:focus-visible::before {
    content: "";
    position: absolute;
    top: 50%;
    left: calc(100% + 2px);
    transform: translateY(-50%);
    border: 6px solid transparent;
    border-right-color: rgba(0, 0, 0, 0.85);
    pointer-events: none;
    z-index: 1100;
  }

  .tile-url-toggle {
    width: 26px;
    height: 26px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    font-size: 12px;
    font-weight: bold;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    line-height: 1;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
  }

  .tile-url-toggle:hover {
    background: white;
    border-color: #999;
  }

  .tile-url-toggle.active {
    background: #93c5fd;
    border-color: #3b82f6;
    color: white;
  }

  /* Map loading styles */
  #map-container {
    opacity: 0;
    transition: opacity 0.3s ease-in-out;
  }

  #map-container.map-loaded {
    opacity: 1;
  }

  .boat-popup {
    position: absolute;
    background: rgba(0, 0, 0, 0.7);
    color: white;
    border: 1px solid #6b7280;
    border-radius: 4px;
    padding: 10px 12px 14px;
    min-width: 130px;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.3);
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    z-index: 1000;
    transform: translate(-50%, -100%);
  }

  .boat-popup.hidden {
    display: none;
  }

  .popup-closer {
    position: absolute;
    top: 6px;
    right: 8px;
    background: none;
    border: none;
    color: rgba(255, 255, 255, 0.4);
    font-size: 12px;
    cursor: pointer;
    padding: 2px;
    line-height: 1;
    transition: color 0.15s;
  }

  .popup-closer:hover {
    color: white;
  }

  .popup-header {
    margin-bottom: 6px;
  }

  .popup-columns {
    display: flex;
    gap: 20px;
    padding: 8px;
  }

  .popup-columns.single-column {
    gap: 12px;
  }

  .popup-detail-slot {
    width: 200px;
    flex-shrink: 0;
  }

  .popup-content {
    display: flex;
    flex-direction: column;
    gap: 3px;
    width: 110px;
    flex-shrink: 0;
  }

  .popup-title {
    font-size: 13px;
    font-weight: 600;
    margin: 0 0 4px 0;
    padding-right: 16px;
    color: #38bdf8;
    letter-spacing: 0.01em;
  }

  .popup-flag {
    margin-right: 4px;
    font-size: 14px;
    /* Strip the title's letter-spacing so the two regional-indicator
       glyphs stay glued into a single flag instead of rendering as a
       pair of country-letter boxes on platforms with emoji flag
       support. */
    letter-spacing: 0;
  }

  .popup-row {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 12px;
  }

  .popup-label {
    color: rgba(255, 255, 255, 0.5);
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  .popup-value {
    font-weight: 500;
    font-size: 12px;
    text-align: right;
    font-variant-numeric: tabular-nums;
    font-family: ui-monospace, monospace;
    text-wrap: nowrap;
  }

  .compass-arrow {
    display: inline-block;
    margin-left: 6px;
    font-size: 14px;
    color: #38bdf8;
    transition: transform 0.3s ease;
  }

  .popup-arrow {
    display: none;
  }

  /* Layer controls panel - hidden by default */
  /* Layers / Boats panels pop out to the right of their toolbar
     buttons in .left-toolbar. Both are anchored at the same
     top:90px (matching .left-toolbar's top); when both are open
     they stack visually side-by-side because the boats panel uses
     a wider left offset. */
  .layer-controls {
    position: absolute;
    top: 90px;
    left: 48px;
    background: rgba(255, 255, 255, 0.95);
    padding: 10px 14px;
    border-radius: 4px;
    font-size: 12px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    z-index: 1003;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    border: 1px solid #ccc;
    display: none;
  }

  /* Show when expanded */
  .layers-expanded .layer-controls {
    display: block;
  }

  .layer-controls > label {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 3px 0;
    cursor: pointer;
    white-space: nowrap;
  }

  .layer-controls > .layer-divider {
    border: 0;
    border-top: 1px solid #ccc;
    margin: 6px 0;
  }

  /* Depth-colour track toggle. Sits below the overlays divider; the
     gradient swatch matches the colour ramp the track itself uses so
     the legend doubles as the visual key. */
  .layer-controls > .depth-color-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 3px 0;
    cursor: pointer;
    white-space: nowrap;
  }
  .depth-color-legend {
    display: inline-block;
    width: 50px;
    height: 8px;
    border-radius: 2px;
    background: linear-gradient(to right, red, green);
  }
  .depth-color-legend-label {
    font-size: 10px;
    color: #6b7280;
  }

  .layer-controls > label.child-layer {
    padding-left: 20px;
  }

  .layer-controls > label.disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .layer-controls > label:hover {
    color: #0066cc;
  }

  .layer-controls input[type="checkbox"] {
    margin: 0;
    width: 14px;
    height: 14px;
    cursor: pointer;
  }

  .layer-controls .heading-line-length {
    margin-left: auto;
    font-size: 11px;
    padding: 1px 2px;
  }

  .stop-panning-btn {
    position: absolute;
    bottom: 45px;
    right: 10px;
    z-index: 1001;
    padding: 18px 36px;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 6px;
    font-size: 36px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    cursor: pointer;
  }

  .stop-panning-btn:hover {
    color: #0066cc;
  }

  .layers-expanded .stop-panning-btn {
    bottom: auto;
    top: 10px;
  }

  /* Layers toggle button */
  .bottom-controls {
    /* desktop: no layout change, children remain absolutely positioned */
    position: static;
  }

  .layers-toggle {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .layers-toggle:hover {
    background: white;
    border-color: #999;
  }

  .layers-toggle.active {
    background: #2563eb;
    color: white;
    border-color: #1d4ed8;
  }

  .layers-toggle.active:hover {
    background: #1d4ed8;
  }

  /* Map-control buttons share a base style and stack vertically inside
     .left-toolbar; per-button rules below only override colour/active
     states. */
  .measure-toggle {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .measure-toggle:hover {
    background: white;
    border-color: #999;
  }

  .measure-toggle.active {
    background: #ff4444;
    color: white;
    border-color: #cc0000;
  }

  .measure-toggle.active:hover {
    background: #ee3333;
  }

  .heads-up-toggle {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .heads-up-toggle:hover:not(:disabled) {
    background: white;
    border-color: #999;
  }

  .heads-up-toggle:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .heads-up-toggle.active {
    background: #2563eb;
    color: white;
    border-color: #1d4ed8;
  }

  .heads-up-toggle.active:hover {
    background: #1d4ed8;
  }

  .boat-position-toggle {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .boat-position-toggle:hover:not(:disabled) {
    background: white;
    border-color: #999;
  }

  .boat-position-toggle:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .boat-position-toggle.active {
    background: #2563eb;
    color: white;
    border-color: #1d4ed8;
  }

  .boat-position-toggle.active:hover {
    background: #1d4ed8;
  }

  .auto-zoom-toggle {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .auto-zoom-toggle:hover {
    background: white;
    border-color: #999;
  }

  .auto-zoom-toggle.active {
    background: #2563eb;
    color: white;
    border-color: #1d4ed8;
  }

  .auto-zoom-toggle.active:hover {
    background: #1d4ed8;
  }

  .add-waypoint-toggle {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .add-waypoint-toggle:hover {
    background: white;
    border-color: #999;
  }

  .add-waypoint-toggle.active {
    background: #f59e0b;
    color: white;
    border-color: #b45309;
  }

  .add-waypoint-toggle.active:hover {
    background: #d97706;
  }

  .clear-waypoints-btn {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #b45309;
    font-size: 14px;
    font-weight: bold;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .clear-waypoints-btn:hover {
    background: #fef3c7;
    border-color: #b45309;
  }

  /* Armed state: red so the second click clearly looks destructive. */
  .clear-waypoints-btn.armed {
    background: #dc2626;
    color: white;
    border-color: #991b1b;
  }

  .clear-waypoints-btn.armed:hover {
    background: #b91c1c;
  }

  /* When the data panel is hidden, the map is full-width. Only
     .measure-result still uses an absolute right-anchored position
     and needs to dodge the page-level fullscreen/drawer buttons. The
     toolbar buttons are now flex children of .left-toolbar, so their
     position is independent of the data panel. */
  #map-container.full-width .measure-result {
    right: calc(10px + 85px);
  }

  .measure-result {
    position: absolute;
    top: calc(10px + 32px + 6px);
    right: 10px;
    bottom: auto;
    padding: 8px 12px;
    background: rgba(15, 23, 42, 0.95);
    color: white;
    border-radius: 4px;
    font-size: 13px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.3);
    z-index: 1000;
    white-space: nowrap;
  }

  .edit-waypoint-popup {
    display: flex;
    gap: 4px;
    align-items: center;
    background: rgba(15, 23, 42, 0.95);
    border: 1px solid #f59e0b;
    border-radius: 6px;
    padding: 4px;
    box-shadow: 0 2px 8px rgba(0, 0, 0, 0.4);
    z-index: 1002;
    white-space: nowrap;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
  }

  .edit-waypoint-popup.delete {
    border-color: #dc2626;
  }

  .edit-waypoint-popup.hidden {
    display: none;
  }

  .edit-waypoint-btn {
    background: #f59e0b;
    color: #1f2937;
    border: none;
    border-radius: 4px;
    padding: 4px 10px;
    font-size: 12px;
    font-weight: 600;
    cursor: pointer;
  }

  .edit-waypoint-btn:hover {
    background: #d97706;
  }

  .edit-waypoint-btn.delete {
    background: #dc2626;
    color: white;
  }

  .edit-waypoint-btn.delete:hover {
    background: #b91c1c;
  }

  .edit-waypoint-close {
    background: transparent;
    color: #fde68a;
    border: none;
    padding: 2px 6px;
    cursor: pointer;
    font-size: 12px;
  }

  .edit-waypoint-close:hover {
    color: white;
  }


  /* Boats panel (bottom-right, next to Layers) */
  .boats-controls {
    display: flex;
    gap: 6px;
    padding: 8px 14px;
    border-top: 1px solid #ddd;
    border-bottom: 1px solid #ddd;
  }

  .select-btn {
    flex: 1;
    padding: 4px 8px;
    font-size: 10px;
    font-weight: 500;
    background: rgba(232, 232, 232, 0.2);
    color: #444;
    border: 1px solid rgba(167, 167, 167, 0.3);
    border-radius: 3px;
    cursor: pointer;
    transition: all 0.2s;
  }

  .select-btn:hover {
    background: rgba(219, 219, 219, 0.3);
    border: 1px solid rgba(167, 167, 167, 0.4);
    color: #444;
  }

  .select-btn:active {
    transform: scale(0.98);
  }

  .boats-panel {
    position: absolute;
    top: 90px;
    /* Offset further right than .layer-controls (which sits at left:48px,
       ~200px wide) so both panels can be open simultaneously without
       overlapping. */
    left: 260px;
    background: rgba(255, 255, 255, 0.95);
    padding: 0;
    border-radius: 4px;
    font-size: 12px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    z-index: 1000;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    border: 1px solid #ccc;
    display: none;
    max-height: 520px;
    width: 200px;
    flex-direction: column;
  }

  .boats-expanded .boats-panel {
    display: flex;
  }

  .boat-search-input {
    width: calc(100% - 28px);
    margin: 8px 14px 0;
    padding: 6px 8px;
    border: 1px solid #ccc;
    border-radius: 3px;
    font-size: 12px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    outline: none;
    background: white;
    box-sizing: border-box;
  }

  .boat-search-input:focus {
    border-color: #0066cc;
    box-shadow: 0 0 0 1px rgba(0, 102, 204, 0.2);
  }

  .boats-list {
    flex: 1;
    overflow-y: auto;
    padding: 6px 14px;
    max-height: 380px;
    -webkit-overflow-scrolling: touch; /* Smooth scrolling on iOS */
  }

  .boat-item {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 0;
    cursor: pointer;
    white-space: nowrap;
    -webkit-tap-highlight-color: transparent; /* Remove tap highlight on mobile */
  }

  .boat-item:active {
    background: rgba(0, 102, 204, 0.1);
  }

  .boat-item input[type="checkbox"] {
    margin: 0;
    width: 18px;
    height: 18px;
    cursor: pointer;
    flex-shrink: 0;
  }

  .boat-name {
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 150px;
  }

  .boat-name.my-boat {
    font-weight: 600;
  }

  .boats-separator {
    font-size: 11px;
    color: #666;
    padding: 8px 0 4px 0;
    margin-top: 4px;
    border-top: 1px solid #ddd;
    font-weight: 500;
  }

  .boat-item.offline .boat-name {
    color: #888;
  }

  .fit-all-btn {
    padding: 6px 12px;
    margin: 8px 14px;
    margin-top: 16px;
    background: #0066cc;
    color: white;
    border: none;
    border-radius: 4px;
    cursor: pointer;
    font-size: 12px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    flex-shrink: 0;
    -webkit-tap-highlight-color: transparent;
    position: relative;
  }

  .fit-all-btn::before {
    content: "";
    position: absolute;
    top: -8px;
    left: -14px;
    right: -14px;
    height: 1px;
    background: #ddd;
  }

  .fit-all-btn:hover {
    background: #0052a3;
  }

  .fit-all-btn:active {
    background: #004080;
  }

  .boats-toggle {
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .boats-toggle:hover {
    background: white;
    border-color: #999;
  }

  .boats-toggle.active {
    background: #2563eb;
    color: white;
    border-color: #1d4ed8;
  }

  .boats-toggle.active:hover {
    background: #1d4ed8;
  }

  @media (max-width: 639px) {
    .boats-panel {
      left: 48px;
      width: calc(100vw - 60px);
      max-width: 320px;
      max-height: 60vh;
    }
  }
</style>
