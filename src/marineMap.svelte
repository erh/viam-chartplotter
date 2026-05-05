<script lang="ts">
  import { onMount } from "svelte";
  import { getCookie, setCookie } from "typescript-cookie";
  import type { BoatInfo, PositionPoint, Detection, DetectionConfig } from "./lib/BoatInfo";
  import RegularShape from "ol/style/RegularShape.js";

  import Collection from "ol/Collection.js";
  import { useGeographic } from "ol/proj.js";
  import { boundingExtent } from "ol/extent.js";
  import "ol/ol.css";
  import ScaleLine from "ol/control/ScaleLine.js";
  import { defaults as defaultControls } from "ol/control/defaults.js";
  import Map from "ol/Map";
  import View from "ol/View";
  import TileLayer from "ol/layer/Tile";
  import Point from "ol/geom/Point.js";
  import LineString from "ol/geom/LineString.js";
  import TileWMS from "ol/source/TileWMS.js";
  import Feature from "ol/Feature.js";
  import VectorSource from "ol/source/Vector.js";
  import { Vector } from "ol/layer.js";
  import XYZ from "ol/source/XYZ";
  import { Circle as CircleStyle, Fill, Icon, Stroke, Style } from "ol/style.js";
  import Overlay from "ol/Overlay.js";
  import { getDistance, offset as sphereOffset } from "ol/sphere.js";
  import type { Geometry } from "ol/geom";
  import type BaseLayer from "ol/layer/Base";
  import type { TileCoord } from "ol/tilecoord";

  interface LayerOption {
    name: string;
    displayName?: string; // Optional display name for UI (defaults to name)
    on: boolean;
    layer: TileLayer<any> | Vector<any>;
    parent?: string; // Parent layer name for hierarchical layers
  }

  let boatImage = "boat3.jpg";

  let popupState = $state({
    overlay: null as Overlay | null,
    visible: false,
    content: {
      name: "",
      mmsi: "",
      speed: 0,
      heading: 0,
      lat: 0,
      lng: 0,
      isMyBoat: false,
      host: undefined as string | undefined,
      partId: undefined as string | undefined,
      isOnline: true,
    },
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

  const COOKIE_HEADS_UP = "mapHeadsUp";
  const COOKIE_LAYERS = "mapLayers";
  const COOKIE_HEADING_LINE_LENGTH = "mapHeadingLineLengthNm";
  const COOKIE_BOAT_POSITION = "mapBoatPosition";
  const COOKIE_OPTS = { expires: 365, sameSite: "lax" as const, path: "/" };

  const HEADING_LINE_LENGTH_OPTIONS = [1, 2, 3, 5, 10, 15];

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
    depthColorTrack = false,
    enableBoatsPanel = false,
    externalVisibilityControl = false,
    showOfflineBoatsInPanel = true,
    defaultAisVisible = true,
    fullWidth = false,
    onReady,
    boatDetailSlot,
    fitBoundsPadding,
    onBoatPopupOpen,
    detectionConfig,
  }: {
    myBoat?: BoatInfo;
    zoomModifier?: number;
    boats?: BoatInfo[];
    positionHistorical?: PositionPoint[];
    depthColorTrack?: boolean;
    enableBoatsPanel?: boolean;
    fullWidth?: boolean;
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
  } = $props();

  // Create derived values for reactivity tracking
  let boatsKey = $derived(
    JSON.stringify(boats?.map((b) => [b.location, b.speed, b.heading, b.positionHistory?.length]))
  );
  let myBoatKey = $derived(
    myBoat ? JSON.stringify([myBoat.heading, myBoat.location, myBoat.speed, myBoat.route]) : null
  );
  let visibleBoatsKey = $derived(JSON.stringify([...visibleBoats]));
  let effectiveVisibleKey = $derived(JSON.stringify([...effectiveVisibleBoats]));

  $effect(() => {
    // Read derived keys to create dependencies
    const _boats = boatsKey;
    const _myBoat = myBoatKey;
    const _visible = visibleBoatsKey;
    updateFromData();
  });

  // Update popup content if it's open and showing a boat that moved
  $effect(() => {
    if (!popupState.visible) return;

    if (popupState.content.isMyBoat && myBoat) {
      // Update my boat popup
      popupState.content.speed = myBoat.speed;
      popupState.content.heading = myBoat.heading;
      popupState.content.lat = myBoat.location[0];
      popupState.content.lng = myBoat.location[1];
    } else if (popupState.content.mmsi && boats) {
      // Update AIS boat popup
      const boat = boats.find((b) => b.mmsi === popupState.content.mmsi);
      if (boat) {
        popupState.content.speed = boat.speed;
        popupState.content.heading = boat.heading;
        popupState.content.lat = boat.location[0];
        popupState.content.lng = boat.location[1];
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
    // Trigger style recalculation by notifying the track layers
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
    mapGlobal.layerOptions.map((l) => ({ name: l.name, on: l.on }));

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
    headingLineFeatures: new Collection<Feature<Geometry>>(),
    trackFeaturesLastCheck: new Date(0),
    myBoatMarker: null as Feature<Point> | null,

    // Track layer references for refreshing styles
    trackLayer: null as Vector<any> | null,
    aisTrackLayer: null as Vector<any> | null,

    layerOptions: [] as LayerOption[],
    onLayers: new Collection<BaseLayer>(),
  });

  let inPanMode = $state(false);

  let mapInternalState: {
    lastZoom: number;
    lastCenter: number[] | null;
    lockedZoom: number | null;
    lastPosition: number[];
    lastPositions: Record<string, number[]>;
    trackFeatureIds: Record<string, boolean>;
    aisTrackFeatureIds: Record<string, boolean>;
    lastPosHistoricalKey: string;
  } = {
    lastZoom: 0,
    lastCenter: null,
    lockedZoom: null,
    lastPosition: [0, 0],
    lastPositions: {},
    trackFeatureIds: {},
    aisTrackFeatureIds: {},
    lastPosHistoricalKey: "",
  };

  function updateFromData() {
    if (!mapGlobal.map || !mapGlobal.view) {
      return;
    }

    if (
      mapInternalState.lastZoom > 0 &&
      mapInternalState.lastCenter != null &&
      mapInternalState.lastCenter[0] != 0
    ) {
      var z = mapGlobal.view.getZoom();
      if (z != mapInternalState.lastZoom) {
        inPanMode = true;
      }

      var c = mapGlobal.view.getCenter();
      if (c) {
        var diff = pointDiff(c, mapInternalState.lastCenter);
        if (diff > 0.003) {
          inPanMode = true;
        }
      }
    }

    var sz = mapGlobal.map.getSize();

    // Update my boat marker if myBoat is provided
    if (myBoat && mapGlobal.myBoatMarker) {
      var pp = [myBoat.location[1], myBoat.location[0]];
      mapGlobal.myBoatMarker.setGeometry(new Point(pp));

      if (!inPanMode && sz) {
        var boatPx: [number, number] =
          boatPositionMode === "bottom" ? [sz[0] / 2, sz[1] * 0.8] : [sz[0] / 2, sz[1] / 2];
        mapGlobal.view.centerOn(pp, sz, boatPx);

        var zoom: number;
        if (mapInternalState.lockedZoom != null) {
          zoom = mapInternalState.lockedZoom;
        } else {
          // zoom of 10 is about 30 miles
          // zoom of 16 is city level
          zoom = Math.pow(Math.floor(myBoat.speed), 0.41);
          zoom = Math.floor(16 - zoom) + (zoomModifier || 0);
          if (zoom <= 0) {
            zoom = 1;
          }
        }
        mapGlobal.view.setZoom(zoom);

        mapInternalState.lastZoom = zoom;
        mapInternalState.lastCenter = pp;
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

        // Track AIS boat position history
        recordTrackPoint(mmsi, boatPos);

        for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
          var v = mapGlobal.aisFeatures.item(i) as Feature<Geometry>;
          if (v.get("mmsi") == mmsi) {
            v.setGeometry(new Point(boatPos));
            v.set("speed", boat.speed);
            v.set("heading", boat.heading);
            v.set("name", boat.name);
            v.set("visible", isVisible);
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
            visible: isVisible,
            geometry: new Point(boatPos),
          })
        );
      });

      for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
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
    depth?: number
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
        })
      );

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

  // Render historical track from position history array
  // Draws dotted 33% transparent lines between points that are 10+ nautical miles apart
  function renderHistoricalTrack(
    boatId: string,
    history: { lat: number; lng: number; depth?: number }[],
    idPrefix: string
  ): void {
    let prev: number[] | null = null;
    let prevPoint: { lat: number; lng: number; depth?: number } | null = null;

    history.forEach((p) => {
      const pp = [p.lng, p.lat];

      if (prev && prevPoint) {
        // Calculate distance between consecutive points
        const distanceNM = calculateDistanceNM(prevPoint.lat, prevPoint.lng, p.lat, p.lng);

        // Mark as gap if points are more than 10 nautical miles apart
        const isGap = distanceNM >= 10;

        addTrackFeature(
          `${idPrefix}-line-${p.lng}-${p.lat}`,
          new LineString([prev, pp]),
          boatId,
          isGap,
          p.depth
        );
      }

      prev = pp;
      prevPoint = p;
    });
  }

  // Create boat icon style
  function createBoatStyle(heading: number, scale: number, visible: boolean): Style {
    if (!visible) {
      return new Style({}); // Empty style = hidden
    }

    const rotation = (heading / 360) * Math.PI * 2;

    return new Style({
      image: new Icon({
        src: boatImage,
        scale: scale,
        rotation: rotation,
        rotateWithView: true,
      }),
    });
  }

  function getTileUrlFunction(
    url: string,
    type: string,
    coordinates: TileCoord
  ): string | undefined {
    var x = coordinates[1];
    var y = coordinates[2];
    var z = coordinates[0];
    var limit = Math.pow(2, z);
    if (y < 0 || y >= limit) {
      return undefined;
    } else {
      x = ((x % limit) + limit) % limit;

      var path = z + "/" + x + "/" + y + "." + type;
      return url + path;
    }
  }

  function toggleMeasure() {
    if (measureActive) {
      stopMeasure();
    } else {
      measureActive = true;
      measurePoints = [];
      measureDistance = null;
      if (measureSource) measureSource.clear();
      closePopup();
    }
  }

  function stopMeasure() {
    measureActive = false;
    measurePoints = [];
    measureDistance = null;
    if (measureSource) measureSource.clear();
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
    const z = mapGlobal.view?.getZoom();
    mapInternalState.lockedZoom = typeof z === "number" ? z : null;
    mapInternalState.lastZoom = 0;
    mapInternalState.lastCenter = [0, 0];
    inPanMode = false;
  }

  function setupLayers() {
    // Explicit zIndex per tile layer so OpenLayers renders in declaration
    // order regardless of toggle/insert sequence. Without this, toggling a
    // layer off and back on can land it on top of layers that should sit
    // above it (e.g. OSM ending up above noaa-local after a reload).
    // core open street maps
    mapGlobal.layerOptions.push({
      name: "open street map",
      on: true,
      layer: new TileLayer({
        opacity: 1,
        preload: Infinity, // Preload all tiles at lower zoom levels
        zIndex: 1,
        source: new XYZ({
          url: "https://tile.openstreetmap.org/{z}/{x}/{y}.png",
          transition: 250, // Faster fade-in
        }),
      }),
    });

    // depth data
    mapGlobal.layerOptions.push({
      name: "depth",
      on: false,
      layer: new TileLayer({
        opacity: 0.7,
        preload: 2,
        zIndex: 2,
        source: new TileWMS({
          url: "https://geoserver.openseamap.org/geoserver/gwc/service/wms",
          params: { LAYERS: "gebco2021:gebco_2021", VERSION: "1.1.1" },
          serverType: "geoserver",
          hidpi: false,
          transition: 300,
        }),
      }),
    });

    // harbors
    mapGlobal.layerOptions.push({
      name: "seamark",
      on: false,
      layer: new TileLayer({
        visible: true,
        maxZoom: 19,
        preload: 2,
        zIndex: 3,
        source: new XYZ({
          tileUrlFunction: function (coordinate) {
            return getTileUrlFunction("https://tiles.openseamap.org/seamark/", "png", coordinate);
          },
          transition: 300,
        }),
        properties: {
          name: "seamarks",
          layerId: 3,
          cookieKey: "SeamarkLayerVisible",
          checkboxId: "checkLayerSeamark",
        },
      }),
    });

    // NOAA's public WMS chart service. Authoritative but slow — kept as a
    // fallback / comparison reference. Always available regardless of where the
    // page is being served from. The `_v` param is ignored by the WMS but
    // changes the browser cache key when tileGenVersion is bumped.
    mapGlobal.layerOptions.push({
      name: "noaa",
      on: false,
      layer: new TileLayer({
        opacity: 0.7,
        preload: 2,
        zIndex: 4,
        source: new TileWMS({
          url: "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer",
          params: { _v: tileGenVersion },
          transition: 300,
        }),
      }),
    });

    // Local ENC renderer — fast, lives at /noaa-enc/tile/{z}/{x}/{y}.png served
    // by the Go module on :8888 (and proxied through Vite on :5173). Only
    // registered when we're being served from one of those ports; elsewhere
    // the path doesn't exist.
    if (noaaCacheReachable()) {
      const params = new URLSearchParams();
      params.set("v", tileGenVersion);
      if (safeDepthParam) params.set("sd", safeDepthParam);
      mapGlobal.layerOptions.push({
        name: "noaa-local",
        on: false,
        layer: new TileLayer({
          opacity: 0.7,
          preload: 2,
          zIndex: 5,
          source: new XYZ({
            url: `/noaa-enc/tile/{z}/{x}/{y}.png?${params.toString()}`,
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
          return createBoatStyle(myBoat.heading, 0.6, effectiveVisibleBoats.has("myBoat"));
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
    }

    var aisLayer = new Vector({
      source: new VectorSource({
        features: mapGlobal.aisFeatures,
      }),
      style: function (feature) {
        const heading = feature.get("heading") ?? 0;
        const visible = feature.get("visible") ?? false;
        return createBoatStyle(heading, 0.35, visible);
      },
      zIndex: 100,
    });

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
  function noaaCacheReachable(): boolean {
    if (typeof window === "undefined") return false;
    const port = window.location.port;
    return port === "5173" || port === "8888";
  }

  // Tracks the last bbox we asked the ENC store to sync so a single-tile pan doesn't
  // spam the backend with overlapping cell-download jobs.
  let lastNoaaPrefetchKey = "";

  function maybePrefetchNoaaTiles() {
    if (!noaaCacheReachable()) return;
    const noaa = findLayerByName("noaa-local");
    if (!noaa || !noaa.on) return;
    if (!mapGlobal.map || !mapGlobal.view) return;

    const size = mapGlobal.map.getSize();
    if (!size) return;
    const extent = mapGlobal.view.calculateExtent(size);
    // Round bbox to ~0.01 deg so trivial pans share a key.
    const round = (n: number) => Math.round(n * 100) / 100;
    const minLon = round(extent[0]);
    const minLat = round(extent[1]);
    const maxLon = round(extent[2]);
    const maxLat = round(extent[3]);
    const key = `${minLon},${minLat},${maxLon},${maxLat}`;
    if (key === lastNoaaPrefetchKey) return;
    lastNoaaPrefetchKey = key;

    fetch("/noaa-enc/prefetch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ minLon, minLat, maxLon, maxLat }),
    }).catch(() => {
      // Best-effort: prefetch failures shouldn't disrupt the UI.
    });
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
    // The marine chart layer skips painting land features (LNDARE/BUAARE) so
    // OSM's land/road/marina detail can show through. That only works if OSM
    // is actually visible, so force OSM on whenever noaa-local is on, even if
    // the user has unchecked the OSM box.
    const noaaLocalLayer = mapGlobal.layerOptions.find((p) => p.name === "noaa-local");
    const noaaLocalOn = !!(noaaLocalLayer && noaaLocalLayer.on);

    for (var l of mapGlobal.layerOptions) {
      var idx = findOnLayerIndexOfName(l.name);

      // Check if parent layer exists and is off
      const parentLayer = l.parent ? mapGlobal.layerOptions.find((p) => p.name === l.parent) : null;
      const isParentOff = parentLayer && !parentLayer.on;

      // Layer should be visible only if it's on AND (has no parent OR parent is on)
      let shouldBeVisible = l.on && !isParentOff;
      if (l.name === "open street map" && noaaLocalOn) {
        shouldBeVisible = true;
      }

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
    maybePrefetchNoaaTiles();
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

  function setupMap() {
    useGeographic();
    setupLayers();

    mapGlobal.view = new View({
      center: [0, 0],
      zoom: 15,
      maxZoom: 19,
    });

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

    // After every pan/zoom settles, ask the NOAA cache to warm tiles around the
    // current viewport. The handler is a no-op when the noaa layer is off.
    mapGlobal.map.on("moveend", () => {
      maybePrefetchNoaaTiles();
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

    // Setup depth tooltip overlay
    const depthTooltipElement = document.getElementById("depth-tooltip");
    const depthTooltipOverlay = new Overlay({
      element: depthTooltipElement || undefined,
      positioning: "bottom-center",
      offset: [0, -10],
    });
    mapGlobal.map.addOverlay(depthTooltipOverlay);

    // Setup measure layer
    measureSource = new VectorSource();
    const measureLayer = new Vector({
      source: measureSource,
      zIndex: 9999,
    });
    mapGlobal.map.addLayer(measureLayer);

    mapClickHandler = function (evt: any) {
      if (measureActive) {
        handleMeasureClick(evt);
        return;
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
          popupState.content = {
            name: feature.get("name") || "Unknown",
            mmsi,
            speed: feature.get("speed") || 0,
            heading: feature.get("heading") || 0,
            lat: coords[1],
            lng: coords[0],
            isMyBoat: false,
            host: boat?.host,
            partId: boat?.partId,
            isOnline: boat?.isOnline ?? false,
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
      mapGlobal.map!.getTargetElement()!.style.cursor = measureActive
        ? "crosshair"
        : hit
          ? "pointer"
          : "";

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
    };
    mapGlobal.map.on("pointermove", mapPointerHandler);

    mapPointerDragHandler = function () {
      inPanMode = true;
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
    setupMap();

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
>
  <div id="map" class="w-full aspect-video bg-white"></div>

  <!-- Boat Info Popup -->
  <div id="boat-popup" class="boat-popup" class:hidden={!popupState.visible}>
    <button class="popup-closer" onclick={closePopup}>✕</button>
    <div class="popup-header">
      <h3 class="popup-title">{popupState.content.name}</h3>
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
        <div class="popup-row">
          <span class="popup-label">LAT</span>
          <span class="popup-value">{formatCoord(popupState.content.lat, true)}</span>
        </div>
        <div class="popup-row">
          <span class="popup-label">LNG</span>
          <span class="popup-value">{formatCoord(popupState.content.lng, false)}</span>
        </div>
      </div>
    </div>
    <div class="popup-arrow"></div>
  </div>

  <!-- Depth Tooltip -->
  <div id="depth-tooltip" class="depth-tooltip"></div>

  {#if inPanMode}
    <button class="stop-panning-btn" onclick={stopPanning}>Stop Panning</button>
  {/if}

  <div class="layer-controls">
    {#each mapGlobal.layerOptions as l, idx}
      {@const parentLayer = l.parent
        ? mapGlobal.layerOptions.find((p) => p.name === l.parent)
        : null}
      {@const isParentOff = parentLayer && !parentLayer.on}
      {@const isOsmForcedByNoaaLocal =
        l.name === "open street map" &&
        !!mapGlobal.layerOptions.find((p) => p.name === "noaa-local")?.on}
      <label class:child-layer={l.parent} class:disabled={isParentOff || isOsmForcedByNoaaLocal}>
        <input
          type="checkbox"
          checked={isOsmForcedByNoaaLocal ? true : mapGlobal.layerOptions[idx].on}
          onchange={(e) => {
            mapGlobal.layerOptions[idx].on = (e.currentTarget as HTMLInputElement).checked;
            saveLayerStates();
            // When the local-NOAA layer is enabled, force a prefetch for the
            // current viewport — otherwise the user has to pan before any
            // cells are downloaded.
            if (l.name === "noaa-local" && mapGlobal.layerOptions[idx].on) {
              lastNoaaPrefetchKey = "";
              maybePrefetchNoaaTiles();
            }
          }}
          disabled={isParentOff || isOsmForcedByNoaaLocal}
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
        {/if}
      </label>
    {/each}
  </div>

  <div class="bottom-controls">
    <button
      class="layers-toggle"
      onclick={() => (layersExpanded = !layersExpanded)}
      aria-label="Toggle map layers"
    >
      {#if layersExpanded}
        ▼ Layers
      {:else}
        ▲ Layers
      {/if}
    </button>

    {#if enableBoatsPanel}
      <!-- Boats Panel (bottom-right, next to Layers) -->
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

      <button
        class="boats-toggle"
        onclick={() => (boatsExpanded = !boatsExpanded)}
        aria-label="Toggle boats panel"
      >
        {#if boatsExpanded}
          ▼ Boats
        {:else}
          ▲ Boats
        {/if}
      </button>
    {/if}
  </div>

  <button
    class="measure-toggle"
    class:active={measureActive}
    onclick={toggleMeasure}
    aria-pressed={measureActive}
    title="Measure distance"
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

  <button
    class="heads-up-toggle"
    class:active={headsUpActive}
    onclick={toggleHeadsUp}
    aria-pressed={headsUpActive}
    disabled={!myBoat}
    title={headsUpActive ? "Heads-up orientation (on)" : "Heads-up orientation (north up)"}
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
    title={boatPositionMode === "bottom"
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

  {#if measureActive && measureDistance !== null}
    <div class="measure-result">
      {measureDistance.toFixed(2)} nm ({(measureDistance * 1.15078).toFixed(2)} mi)
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
    background: rgba(15, 23, 42, 0.95);
    backdrop-filter: blur(8px);
    color: white;
    border-radius: 4px;
    padding: 10px 12px 14px;
    min-width: 130px;
    box-shadow: 0 4px 20px rgba(0, 0, 0, 0.5);
    border: 1px solid rgba(255, 255, 255, 0.08);
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
    position: absolute;
    bottom: -5px;
    left: 50%;
    transform: translateX(-50%);
    width: 0;
    height: 0;
    border-left: 5px solid transparent;
    border-right: 5px solid transparent;
    border-top: 5px solid rgba(15, 23, 42, 0.95);
  }

  /* Layer controls panel - hidden by default */
  .layer-controls {
    position: absolute;
    bottom: 45px;
    right: 10px;
    background: rgba(255, 255, 255, 0.95);
    padding: 10px 14px;
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
    padding: 6px 12px;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    font-size: 12px;
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
    position: absolute;
    bottom: 10px;
    right: 10px;
    padding: 6px 12px;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    font-size: 12px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
  }

  .layers-toggle:hover {
    background: white;
    border-color: #999;
  }

  /* Measure toggle button (top-right, all screen sizes) */
  .measure-toggle {
    position: absolute;
    top: 10px;
    right: 10px;
    bottom: auto;
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
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
    position: absolute;
    top: 10px;
    right: calc(10px + 30px + 6px);
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
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
    position: absolute;
    top: 10px;
    right: calc(10px + 30px + 6px + 30px + 6px);
    width: 30px;
    height: 30px;
    padding: 0;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
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

  /* When the data panel is hidden, the map is full-width and its top-right
     buttons collide with the page-level fullscreen / drawer-toggle buttons.
     Shift the map's top-right cluster left to clear them. */
  #map-container.full-width .measure-toggle {
    right: calc(10px + 85px);
  }
  #map-container.full-width .heads-up-toggle {
    right: calc(10px + 30px + 6px + 85px);
  }
  #map-container.full-width .boat-position-toggle {
    right: calc(10px + 30px + 6px + 30px + 6px + 85px);
  }
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
    bottom: 45px;
    right: calc(10px + 70px + 0.5rem); /* Match boats-toggle position */
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

  /* Boats toggle button (bottom-right, next to Layers) */
  .boats-toggle {
    position: absolute;
    bottom: 10px;
    right: calc(10px + 70px + 0.5rem); /* 10px margin + layers button + gap */
    padding: 6px 12px;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    font-size: 12px;
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
  }

  .boats-toggle:hover {
    background: white;
    border-color: #999;
  }

  @media (max-width: 639px) {
    .bottom-controls {
      position: absolute;
      bottom: 10px;
      right: 10px;
      left: auto;
      display: flex;
      gap: 8px;
      z-index: 1001;
      align-items: center;
    }

    .bottom-controls .layers-toggle,
    .bottom-controls .boats-toggle {
      position: static;
    }

    .boats-panel {
      right: 10px;
      left: auto;
      width: calc(100vw - 20px);
      max-width: 320px;
      max-height: 60vh;
    }
  }
</style>
