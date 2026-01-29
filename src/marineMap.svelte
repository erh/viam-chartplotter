<script lang="ts">

 import { onMount } from 'svelte';
import type { BoatInfo, PositionPoint, Detection } from './lib/BoatInfo';
 import RegularShape from 'ol/style/RegularShape.js';

 import Collection from 'ol/Collection.js';
 import {useGeographic} from 'ol/proj.js';
 import {boundingExtent} from 'ol/extent.js';
 import 'ol/ol.css';
 import ScaleLine from 'ol/control/ScaleLine.js';
 import {defaults as defaultControls} from 'ol/control/defaults.js';
 import Map from 'ol/Map';
 import View from 'ol/View';
 import TileLayer from 'ol/layer/Tile';
 import Point from 'ol/geom/Point.js';
 import LineString from 'ol/geom/LineString.js';
 import TileWMS from 'ol/source/TileWMS.js';
 import Feature from 'ol/Feature.js';
 import VectorSource from 'ol/source/Vector.js';
 import {Vector, Tile} from 'ol/layer.js';
 import XYZ from 'ol/source/XYZ';
 import {
   Circle as CircleStyle,
   Fill,
   Icon,
   Stroke,
   Style,
 } from 'ol/style.js';
 import Overlay from "ol/Overlay.js";
 import type { Geometry } from 'ol/geom';
 import type BaseLayer from 'ol/layer/Base';
 import type { TileCoord } from 'ol/tilecoord';

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
 let showDetections = $state(false);

 // Track which boats are visible (by mmsi, plus 'myBoat' for own boat)
 // When externalVisibilityControl is true, start with empty set (parent will control)
 let visibleBoats = $state<Set<string>>(new Set(['myBoat']));

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
     boatsList?.forEach(b => {
       if (b.mmsi && !visibleBoats.has(b.mmsi)) {
         visibleBoats.add(b.mmsi);
       }
     });
     lastBoatsLength = currentLength; // Plain JS variable, won't re-trigger
     visibleBoats = new Set(visibleBoats); // Trigger reactivity
   }
 });

 // Effect to notify parent when detections checkbox is toggled
 $effect(() => {
   // Pass the current boat's partId so the parent can filter by boat
   const boatPartId = popupState.content.partId || popupState.content.mmsi;
   onShowDetections?.(showDetections, boatPartId);
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
   if (myBoat) allIds.add('myBoat');
   boats?.forEach(b => {
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
   return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180 && 
          !(lat === 0 && lng === 0); // Exclude null island
 }

 function fitToVisibleBoats() {
   if (!mapGlobal.map || !mapGlobal.view) return;

   const coords: number[][] = [];

   // Add my boat if visible and has valid coordinates
   if (myBoat && visibleBoats.has('myBoat') && isValidCoordinate(myBoat.location[0], myBoat.location[1])) {
     coords.push([myBoat.location[1], myBoat.location[0]]); // [lng, lat]
   }

   // Add visible AIS boats with valid coordinates
   boats?.forEach(boat => {
     if (boat.mmsi && visibleBoats.has(boat.mmsi) && isValidCoordinate(boat.location[0], boat.location[1])) {
       coords.push([boat.location[1], boat.location[0]]);
     }
   });

   if (coords.length === 0) return;

   if (coords.length === 1) {
     // Single boat - center on it with reasonable zoom
     mapGlobal.view.animate({
       center: coords[0],
       zoom: Math.min(12, Math.max(8, mapGlobal.view.getZoom() ?? 10)),
       duration: 500
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
     
     if (typeof fitBoundsPadding === 'number') {
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
       maxZoom: 12
     });
   }

   mapInternalState.inPanMode = true; // Prevent auto-centering
 }

 let { myBoat, zoomModifier, boats, positionHistorical, enableBoatsPanel = false, externalVisibilityControl = false, showOfflineBoatsInPanel = true, onReady, boatDetailSlot, fitBoundsPadding, onShowDetections, detections, detectionsLabel = "Show Detections", detectionsLayerName = "detections", detectionsLayerDisplayName = "detections" }: {
  myBoat?: BoatInfo;
  zoomModifier?: number;
  boats?: BoatInfo[];
  positionHistorical?: PositionPoint[];
  enableBoatsPanel?: boolean;
  /** When true, parent controls visibility via setVisibleBoats API instead of auto-showing new boats */
  externalVisibilityControl?: boolean;
  /** When false, offline boats are hidden from the boats panel (default: true) */
  showOfflineBoatsInPanel?: boolean;
  onReady?: (api: {
    fitToVisibleBoats: () => void;
    selectAllBoats: () => void;
    deselectAllBoats: () => void;
    setVisibleBoats: (ids: Set<string>) => void;
    getVisibleBoats: () => Set<string>;
  }) => void;
  boatDetailSlot?: (boat: { host?: string; partId?: string; name: string }) => any;
  fitBoundsPadding?: number | { top?: number; right?: number; bottom?: number; left?: number };
  /** Callback when detections checkbox is toggled */
  onShowDetections?: (enabled: boolean, boatPartId?: string) => void;
  /** Detections to display on the map */
  detections?: Detection[];
  /** Label for detections checkbox in popup (default: "Show Detections") */
  detectionsLabel?: string;
  /** Internal layer name for detections (default: "detections") */
  detectionsLayerName?: string;
  /** Display name for detections layer in layer controls (default: "detections") */
  detectionsLayerDisplayName?: string;
} = $props();

 // Create derived values for reactivity tracking
 let boatsKey = $derived(JSON.stringify(boats?.map(b => [b.location, b.speed, b.heading, b.positionHistory?.length])));
 let myBoatKey = $derived(myBoat ? JSON.stringify([myBoat.heading, myBoat.location, myBoat.speed, myBoat.route]) : null);
 let visibleBoatsKey = $derived(JSON.stringify([...visibleBoats]));

 $effect( () => {
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
     const boat = boats.find(b => b.mmsi === popupState.content.mmsi);
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
     ? !visibleBoats.has('myBoat')
     : popupState.content.mmsi && (
         !boats?.some(b => b.mmsi === popupState.content.mmsi) || 
         !visibleBoats.has(popupState.content.mmsi)
       );
   
   if (shouldClose) closePopup();
 });

 // Force track layers to re-render when visibility changes
 $effect(() => {
   const _visible = visibleBoatsKey;
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

 // Render detections when the prop changes
 $effect(() => {
   // Only render if map is loaded and we have detections
   if (mapLoaded) {
     renderDetections(detections);
   }
 });

 let mapGlobal = $state({

   map: null as Map | null,
   view: null as View | null,

   aisFeatures: new Collection<Feature<Geometry>>(),
   trackFeatures: new Collection<Feature<Geometry>>(),
   aisTrackFeatures: new Collection<Feature<Geometry>>(),
   routeFeatures: new Collection<Feature<Geometry>>(),
   detectionFeatures: new Collection<Feature<Geometry>>(),
   trackFeaturesLastCheck : new Date(0),
   myBoatMarker: null as Feature<Point> | null,
   
   // Track layer references for refreshing styles
   trackLayer: null as Vector<any> | null,
   aisTrackLayer: null as Vector<any> | null,

   layerOptions: [] as LayerOption[],
   onLayers: new Collection<BaseLayer>(),
 });

 let mapInternalState: {
   inPanMode: boolean;
   lastZoom: number;
   lastCenter: number[] | null;
   lastPosition: number[];
   lastPositions: Record<string, number[]>;
   trackFeatureIds: Record<string, boolean>;
   aisTrackFeatureIds: Record<string, boolean>;
 } = {
   inPanMode: false,
   lastZoom: 0,
   lastCenter: null,
   lastPosition: [0,0],
   lastPositions: {},
   trackFeatureIds: {},
   aisTrackFeatureIds: {},
 }

 function updateFromData() {
   if (!mapGlobal.map || !mapGlobal.view) {
     return
   }

   if (mapInternalState.lastZoom > 0 && mapInternalState.lastCenter != null && mapInternalState.lastCenter[0] != 0 ) {
     var z = mapGlobal.view.getZoom();
     if (z != mapInternalState.lastZoom) {
       mapInternalState.inPanMode = true;
     }
     
     var c = mapGlobal.view.getCenter();
     if (c) {
       var diff = pointDiff(c, mapInternalState.lastCenter);
       if (diff > .003) {
         mapInternalState.inPanMode = true;
       }
     }
   }
   
   var sz = mapGlobal.map.getSize();
   
   // Update my boat marker if myBoat is provided
   if (myBoat && mapGlobal.myBoatMarker) {
     var pp = [myBoat.location[1], myBoat.location[0]];
     mapGlobal.myBoatMarker.setGeometry(new Point(pp));
     
     if (!mapInternalState.inPanMode && sz) {
       mapGlobal.view.centerOn(pp, sz, [sz[0]/2,sz[1]/2]);
       
       // zoom of 10 is about 30 miles
       // zoom of 16 is city level
       var zoom = Math.pow(Math.floor(myBoat.speed),.41)
       zoom = Math.floor(16-zoom) + (zoomModifier||0);
       if ( zoom <= 0 ) {
         zoom = 1;
       }
       //console.log("speed: " + myBoat.speed + " zoom: " + zoom);
       mapGlobal.view.setZoom(zoom);
       
       mapInternalState.lastZoom = zoom;
       mapInternalState.lastCenter = pp;
     }
     
     if (pp[0] != 0) {
       recordTrackPoint("myBoat", pp);
     }
   }
   
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
     boats.forEach( (boat) => {

       var mmsi = boat.mmsi;
       if (!mmsi) {
         return;
       }
       seen[mmsi] = true;
       const isVisible = visibleBoats.has(mmsi);
       
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

       mapGlobal.aisFeatures.push(new Feature({
         type: "ais",
         name: boat.name,
         mmsi: mmsi,
         speed: boat.speed,
         heading: boat.heading,
         visible: isVisible,
         geometry: new Point(boatPos),
       }));
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

   // Render historical tracks
   if (positionHistorical) {
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
       var x = mapGlobal.trackFeatures.item(0);
       if (x) {
         delete mapInternalState.trackFeatureIds[x.get("myid")];
       }
       mapGlobal.trackFeatures.removeAt(0);
     }
   }
   
   // Remove oldest features if over limit (AIS track)
   if (mapGlobal.aisTrackFeatures.getLength() > maxFeatures) {
     const toRemove = mapGlobal.aisTrackFeatures.getLength() - maxFeatures;
     for (let i = 0; i < toRemove; i++) {
       var x = mapGlobal.aisTrackFeatures.item(0);
       if (x) {
         delete mapInternalState.aisTrackFeatureIds[x.get("myid")];
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
     type: isAis ? "ais-track" : "track"
   };
 }

 // Create style for detection markers
 function createDetectionStyle(): Style {
   return new Style({
     image: new RegularShape({
       stroke: new Stroke({ color: 'white', width: 2 }),
       points: 3,
       radius: 10,
       rotation: 0,
       angle: 0,
     }),
   });
 }

 // Find the interpolated position on a track at a given timestamp
 function interpolatePositionAtTime(
   history: PositionPoint[],
   targetTime: Date
 ): { lat: number; lng: number } | null {
   if (!history || history.length === 0) return null;

   // Filter points that have timestamps
   const timedPoints = history.filter((p) => p.ts !== undefined);
   if (timedPoints.length === 0) {
     console.warn(`No timed points in history of ${history.length} points`);
     return null;
   }

   const targetMs = targetTime.getTime();

   // Find the two points that bracket the target time
   let before: PositionPoint | null = null;
   let after: PositionPoint | null = null;

   for (let i = 0; i < timedPoints.length; i++) {
     const pointTime = timedPoints[i].ts!.getTime();

     if (pointTime <= targetMs) {
       before = timedPoints[i];
     }
     if (pointTime >= targetMs && after === null) {
       after = timedPoints[i];
       break;
     }
   }

   // If target is before all points, use the first point
   if (before === null && after !== null) {
     return { lat: after.lat, lng: after.lng };
   }

   // If target is after all points, use the last point
   if (after === null && before !== null) {
     return { lat: before.lat, lng: before.lng };
   }

   // Both null? shouldn't happen
   if (before === null || after === null) {
     console.error(`Failed to find bracketing points for ${targetTime.toISOString()}`);
     return null;
   }

   // If exact match (both same point)
   if (before === after) {
     return { lat: before.lat, lng: before.lng };
   }

   // Interpolate between the two points
   const beforeMs = before.ts!.getTime();
   const afterMs = after.ts!.getTime();
   const fraction = (targetMs - beforeMs) / (afterMs - beforeMs);

   const lat = before.lat + (after.lat - before.lat) * fraction;
   const lng = before.lng + (after.lng - before.lng) * fraction;

   return { lat, lng };
 }

 // Render detection markers on the map
 function renderDetections(detections: Detection[] | undefined) {
   // Get the layer's source directly to bypass Svelte proxy issues
   const detectionLayer = findLayerByName(detectionsLayerName)?.layer as Vector<any> | undefined;
   const source = detectionLayer?.getSource();

   if (!source) {
     console.warn('Detection layer source not found');
     return;
   }

   // Clear existing features
   source.clear();

   if (!detections || detections.length === 0) return;

   // Collect all position histories from all boats (use plain object to avoid Svelte proxy issues)
   const allHistories: Record<string, PositionPoint[]> = {};

   // Add my boat's history
   if (positionHistorical && positionHistorical.length > 0) {
     allHistories['myBoat'] = positionHistorical;
   }

   // Add each AIS boat's history
   boats?.forEach((boat) => {
     if (boat.positionHistory && boat.positionHistory.length > 0) {
       const key = boat.mmsi || boat.partId || 'unknown';
       allHistories[key] = boat.positionHistory;
     }
   });

   // Collect all features first, then add in batch
   const features: Feature<Point>[] = [];

   // For each detection, find its position and create a marker
   detections.forEach((detection) => {
     let position: { lat: number; lng: number } | null = null;

     // If detection has a specific boat ID, use that boat's history
     if (detection.boatId && allHistories[detection.boatId]) {
       position = interpolatePositionAtTime(allHistories[detection.boatId], detection.timestamp);
     } else {
       // Try all boat histories and use the first one that returns a valid position
       for (const boatId of Object.keys(allHistories)) {
         position = interpolatePositionAtTime(allHistories[boatId], detection.timestamp);
         if (position) break;
       }
     }

     if (position) {
       const feature = new Feature({
         type: 'detection',
         detectionId: detection.id,
         timestamp: detection.timestamp,
         geometry: new Point([position.lng, position.lat]),
       });

       features.push(feature);
     }
   });

   // Add all features at once to the source
   source.addFeatures(features);
 }

 // Factory to create track style functions (DRY for myBoat and AIS tracks)
 function createTrackStyleFunction(defaultBoatId: string) {
   return function(feature: any) {
     const boatId = feature.get("boatId") || defaultBoatId;
     if (!visibleBoats.has(boatId)) {
       return new Style({}); // Hidden - return empty style
     }
     
     const isGap = feature.get("isGap");
     const opacity = isGap ? 0.33 : 1.0;
     
     return new Style({
       stroke: new Stroke({
         color: `rgba(0, 0, 255, ${opacity})`,
         width: 2,
         lineDash: isGap ? [2, 6] : undefined
       }),
     });
   };
 }

 function addTrackFeature(id: string, g: Geometry, boatId: string = "myBoat", isGap: boolean = false) {
   const { featureIds, features, type } = getTrackCollections(boatId);
   
   if (featureIds[id] == true) {
     return;
   }

   featureIds[id] = true;
   
   features.push(new Feature({
     type: type,
     boatId: boatId,
     "myid" : id,
     geometry: g,
     isGap: isGap,
   }));
   
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
   if (diff > .0000001) {
     const { features, type } = getTrackCollections(boatId);
     
     features.push(new Feature({
       type: type,
       boatId: boatId,
       geometry: new LineString([lastPos, position]),
     }));
     
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
   const dLat = (lat2 - lat1) * Math.PI / 180;
   const dLng = (lng2 - lng1) * Math.PI / 180;
   const a = Math.sin(dLat / 2) * Math.sin(dLat / 2) +
             Math.cos(lat1 * Math.PI / 180) * Math.cos(lat2 * Math.PI / 180) *
             Math.sin(dLng / 2) * Math.sin(dLng / 2);
   const c = 2 * Math.atan2(Math.sqrt(a), Math.sqrt(1 - a));
   return R * c;
 }

 // Render historical track from position history array
 // Draws dotted 33% transparent lines between points that are 10+ nautical miles apart
 function renderHistoricalTrack(
   boatId: string, 
   history: { lat: number; lng: number }[], 
   idPrefix: string
 ): void {
   let prev: number[] | null = null;
   let prevPoint: { lat: number; lng: number } | null = null;
   
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
         isGap
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
     }),
   });
 }
 
 function getTileUrlFunction(url: string, type: string, coordinates: TileCoord): string | undefined {
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

 function stopPanning() {
   mapInternalState.lastZoom = 0;
   mapInternalState.lastCenter = [0,0];
   mapInternalState.inPanMode = false;
 }

 function setupLayers() {

   // core open street maps
   mapGlobal.layerOptions.push( {
     name : "open street map",
     on : true,
     layer : new TileLayer({
       opacity: 1,
       preload: Infinity, // Preload all tiles at lower zoom levels
       source: new XYZ({
         url: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png',
         transition: 250, // Faster fade-in
       })
     }),
   })
   
   // depth data
   mapGlobal.layerOptions.push({
     name: "depth",
     on: false,
     layer: new TileLayer({
       opacity: .7,
       preload: 2,
       source: new TileWMS({
         url: 'https://geoserver.openseamap.org/geoserver/gwc/service/wms',
         params: {'LAYERS': 'gebco2021:gebco_2021', 'VERSION':'1.1.1'},
         serverType: 'geoserver',
         hidpi: false,
         transition: 300,
       }),
     }),
   })
   
   // harbors
   mapGlobal.layerOptions.push({
     name: "seamark",
     on: false,
     layer : new TileLayer({
       visible: true,
       maxZoom: 19,
       preload: 2,
       source: new XYZ({
         tileUrlFunction: function(coordinate) {
           return getTileUrlFunction("https://tiles.openseamap.org/seamark/", 'png', coordinate);
         },
         transition: 300,
       }),
       properties: {
         name: "seamarks",
         layerId: 3,
         cookieKey: "SeamarkLayerVisible",
         checkboxId: "checkLayerSeamark",
       }
     }),
   });
   
   mapGlobal.layerOptions.push({
     name: "noaa",
     on: false,
     layer: new TileLayer({
       opacity: .7,
       preload: 2,
       source: new TileWMS({
         url: "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer",
         params: {},
         transition: 300,
       }),
     }),
   })

   // Track layer for myBoat (child of boat layer)
   var trackLayer = new Vector({
     source: new VectorSource({
       features: mapGlobal.trackFeatures,
     }),
     style: createTrackStyleFunction("myBoat"),
   });

   // Store reference for style refreshing
   mapGlobal.trackLayer = trackLayer;

   // AIS Track layer (child of ais layer)
   var aisTrackLayer = new Vector({
     source: new VectorSource({
       features: mapGlobal.aisTrackFeatures,
     }),
     style: createTrackStyleFunction(""),
   });

   // Store reference for style refreshing
   mapGlobal.aisTrackLayer = aisTrackLayer;

   // by boat setup (only if myBoat is provided)
   if (myBoat) {
     mapGlobal.myBoatMarker = new Feature({
       type: 'geoMarker',
       header: 0,
       geometry: new Point([0,0]),
     });
     
     var myBoatFeatures = new Collection<Feature<Geometry>>();
     myBoatFeatures.push(mapGlobal.myBoatMarker);

     var myBoatLayer = new Vector({
       source: new VectorSource({
         features: myBoatFeatures,
       }),
       style: function (feature) {
         return createBoatStyle(myBoat.heading, 0.6, visibleBoats.has('myBoat'));
       },
     });
     mapGlobal.layerOptions.push({
       name: "boat",
       on: true,
       layer : myBoatLayer,
     });
     
     // Track layer - child of boat
     mapGlobal.layerOptions.push({
       name: "track",
       on: true,
       layer: trackLayer,
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
           width: 3
         }),
         fill: new Fill({
           color: "rgba(0, 255, 0, 0.1)"
         })
       }),
     });

     mapGlobal.layerOptions.push({
       name: "route",
       on: true,
       layer : routeLayer,
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
   });

   mapGlobal.layerOptions.push({
     name: "ais",
     on: true,
     layer : aisLayer,
   });
   
   // AIS Track layer - child of ais
   mapGlobal.layerOptions.push({
     name: "ais-track",
     displayName: "track",
     on: true,
     layer: aisTrackLayer,
     parent: "ais",
   });

   // Detections layer
   var detectionLayerVar = new Vector({
     source: new VectorSource({
       features: mapGlobal.detectionFeatures,
     }),
     style: createDetectionStyle(),
   });

   mapGlobal.layerOptions.push({
     name: detectionsLayerName,
     displayName: detectionsLayerDisplayName,
     on: true,
     layer: detectionLayerVar,
   });

 }

 function findLayerByName(name: string): LayerOption | null {
   for( var l of mapGlobal.layerOptions) {
     if (l.name == name) {
       return l;
     }
   }
   return null;
 }

 function findOnLayerIndexOfName(name: string): number {
   var l = findLayerByName(name);
   if (l == null) {
     return -2;
   }

   for ( var i=0; i<mapGlobal.onLayers.getLength(); i++) {
     if ((mapGlobal.onLayers.item(i) as any).ol_uid == (l.layer as any).ol_uid) {
       return i;
     }
   }
   return -1;
 }
 
 function updateOnLayers() {
   for( var l of mapGlobal.layerOptions) {
     var idx = findOnLayerIndexOfName(l.name);
     
     // Check if parent layer exists and is off
     const parentLayer = l.parent ? mapGlobal.layerOptions.find(p => p.name === l.parent) : null;
     const isParentOff = parentLayer && !parentLayer.on;
     
     // Layer should be visible only if it's on AND (has no parent OR parent is on)
     const shouldBeVisible = l.on && !isParentOff;
     
     if (shouldBeVisible) {
       if ( idx < 0 ) {
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
       if ( idx >= 0 ) {
         mapGlobal.onLayers.removeAt(idx);
       }
     }
   }
 }
 
 function pointDiff(x: number[], y: number[]): number {
   var a = x[0] - y[0];
   var b = x[1] - y[1];
   var c = a*a + b*b;
   return Math.sqrt(c);
 }

 // Store event handler references for cleanup (outside setupMap so they're accessible in onMount cleanup)
 let mapClickHandler: any = null;
 let mapPointerHandler: any = null;

 function setupMap() {
   useGeographic();
   setupLayers();
   
   mapGlobal.view = new View({
     center: [0, 0],
     zoom: 15
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
     target: 'map',
     layers: mapGlobal.onLayers as Collection<BaseLayer>,
     view: mapGlobal.view,
     controls: defaultControls().extend([scaleThing])
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

   // Click handler for boat features
   mapClickHandler = function (evt: any) {
     const feature = mapGlobal.map!.forEachFeatureAtPixel(
       evt.pixel,
       function (f) {
         const type = f.get("type");
         if (type === "ais" || type === "geoMarker") {
           return f;
         }
         return null;
       }
     );

     if (feature) {
       const type = feature.get("type");
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
         const boat = boats?.find(b => b.mmsi === mmsi);
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
     } else {
       closePopup();
     }
   };
   mapGlobal.map.on("click", mapClickHandler);

   // Change cursor on hover over boats
   mapPointerHandler = function (evt: any) {
     const hit = mapGlobal.map!.hasFeatureAtPixel(evt.pixel, {
       layerFilter: (layer) => {
         return (
           (layer as any)
             .getSource()
             ?.getFeatures?.()
             ?.some?.(
               (f: Feature) => f.get("type") === "ais" || f.get("type") === "geoMarker"
             ) ?? false
         );
       },
     });
     mapGlobal.map!.getTargetElement()!.style.cursor = hit ? "pointer" : "";
   };
   mapGlobal.map.on("pointermove", mapPointerHandler);

   console.log("setupMap finished");
   
   // Initial fit to show all boats with room for popups (only when boats panel enabled)
   setTimeout(() => {
     mapGlobal.map?.updateSize(); // Ensure map has correct dimensions
     if (enableBoatsPanel && boats && boats.length > 0) {
       fitToVisibleBoats();
     }
     // Expose API to parent component
     onReady?.({ 
       fitToVisibleBoats,
       selectAllBoats,
       deselectAllBoats,
       setVisibleBoats,
       getVisibleBoats: () => new Set(visibleBoats)
     });
   }, 100);
 }

 function closePopup() {
   popupState.visible = false;
   if (popupState.overlay) {
     popupState.overlay.setPosition(undefined);
   }
 }

 function formatCoord(val: number, isLat: boolean): string {
   const dir = isLat ? (val >= 0 ? "N" : "S") : val >= 0 ? "E" : "W";
   return Math.abs(val).toFixed(4) + "° " + dir;
 }

 function handleMapContainerClick(event: MouseEvent) {
   const target = event.target as HTMLElement;
   
   // Close boats panel if clicking outside of it
   if (boatsExpanded) {
     const boatsPanel = target.closest('.boats-panel');
     const boatsToggle = target.closest('.boats-toggle');
     
     if (!boatsPanel && !boatsToggle) {
       boatsExpanded = false;
     }
   }
   
   // Close layers panel if clicking outside of it
   if (layersExpanded) {
     const layersPanel = target.closest('.layer-controls');
     const layersToggle = target.closest('.layers-toggle');
     
     if (!layersPanel && !layersToggle) {
       layersExpanded = false;
     }
   }
 }

 onMount(() => {
   setupMap();
   
   // Listen for initial render complete to fade in map
   if (mapGlobal.map) {
     mapGlobal.map.once('rendercomplete', () => {
       mapLoaded = true;
     });
     // Fallback in case rendercomplete doesn't fire
     setTimeout(() => {
       mapLoaded = true;
     }, 1000);
   }
   
   // Add click-outside handler for panels
   const container = document.getElementById('map-container');
   if (container) {
     container.addEventListener('click', handleMapContainerClick as EventListener);
   }
   
   // Cleanup on unmount
   return () => {
     if (container) {
       container.removeEventListener('click', handleMapContainerClick as EventListener);
     }
     
     // Remove OpenLayers map event listeners to prevent memory leaks
     if (mapGlobal.map) {
       if (mapClickHandler) {
         mapGlobal.map.un("click", mapClickHandler);
       }
       if (mapPointerHandler) {
         mapGlobal.map.un("pointermove", mapPointerHandler);
       }
     }
   };
 });

</script>

<div id="map-container" class="relative lg:col-span-3 row-span-3 lg:row-span-5 border border-dark" class:layers-expanded={layersExpanded} class:boats-expanded={boatsExpanded} class:map-loaded={mapLoaded}>
  <div id="map" class="w-full aspect-video bg-white"></div>

  <!-- Boat Info Popup -->
  <div id="boat-popup" class="boat-popup" class:hidden={!popupState.visible}>
    <button class="popup-closer" onclick={closePopup}>✕</button>
    <div class="popup-header">
      <h3 class="popup-title">{popupState.content.name}</h3>
    </div>
    <div class="popup-columns" class:single-column={!popupState.content.isOnline}>
      {#if boatDetailSlot && popupState.content.host && popupState.content.partId && popupState.content.isOnline}
        <div class="popup-detail-slot">
          {@render boatDetailSlot({ host: popupState.content.host, partId: popupState.content.partId, name: popupState.content.name })}
        </div>
      {/if}
      <div class="popup-content">
        <div class="popup-row">
          <span class="popup-label">SPD</span>
          <span class="popup-value">{popupState.content.speed.toFixed(1)} kn</span>
        </div>
        <div class="popup-row">
          <span class="popup-label">HDG</span>
          <span class="popup-value">{popupState.content.heading.toFixed(0)}°<span class="compass-arrow" style="transform: rotate({popupState.content.heading}deg)">↑</span></span>
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
    <label class="popup-checkbox">
      <input type="checkbox" bind:checked={showDetections} />
      {detectionsLabel}
    </label>
    <div class="popup-arrow"></div>
  </div>

  <div class="layer-controls">
    {#if mapInternalState.inPanMode}
      <div>
        <button onclick={stopPanning}>Stop Panning</button>
      </div>
    {/if}
    {#each mapGlobal.layerOptions as l, idx}
      {@const parentLayer = l.parent ? mapGlobal.layerOptions.find(p => p.name === l.parent) : null}
      {@const isParentOff = parentLayer && !parentLayer.on}
      <label class:child-layer={l.parent} class:disabled={isParentOff}>
        <input 
          type="checkbox" 
          bind:checked={mapGlobal.layerOptions[idx].on}
          disabled={isParentOff}
        >
        {l.displayName || l.name}
      </label>
    {/each}
  </div>

  <button 
    class="layers-toggle"
    onclick={() => layersExpanded = !layersExpanded}
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
      <button class="select-btn" onclick={selectAllBoats} title="Select all boats">Select all</button>
      <button class="select-btn" onclick={deselectAllBoats} title="Deselect all boats">Deselect all</button>
    </div>
    <div class="boats-list">
      {#if myBoat}
      <label class="boat-item">
        <input 
          type="checkbox" 
          checked={visibleBoats.has('myBoat')}
          onchange={() => toggleBoatVisibility('myBoat')}
        >
        <span class="boat-name my-boat">My Boat</span>
      </label>
      {/if}
      {#if boats}
        {@const onlineBoats = boats.filter(b => b.mmsi && b.isOnline !== false)}
        {@const offlineBoats = boats.filter(b => b.mmsi && b.isOnline === false)}
        {#each onlineBoats as boat}
          <label class="boat-item">
            <input 
              type="checkbox" 
              checked={visibleBoats.has(boat.mmsi!)}
              onchange={() => toggleBoatVisibility(boat.mmsi!)}
            >
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
              >
              <span class="boat-name">{boat.name}</span>
            </label>
          {/each}
        {/if}
      {/if}
    </div>
    <button class="fit-all-btn" onclick={fitToVisibleBoats}>
      Fit All Visible
    </button>
  </div>

  <button 
    class="boats-toggle"
    onclick={() => boatsExpanded = !boatsExpanded}
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

<style>
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
    gap: 0;
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
    font-family: system-ui, -apple-system, sans-serif;
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

  /* Layers toggle button */
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
    font-family: system-ui, -apple-system, sans-serif;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
  }

  .layers-toggle:hover {
    background: white;
    border-color: #999;
  }

  /* Boats panel (bottom-right, next to Layers) */
  .boats-controls {
    display: flex;
    gap: 6px;
    padding: 6px 6px;
    border-bottom: 1px solid rgba(0, 0, 0, 0.1);
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
    right: calc(10px + 70px + 1rem); /* Match boats-toggle position */
    background: rgba(255, 255, 255, 0.95);
    padding: 0;
    border-radius: 4px;
    font-size: 12px;
    font-family: system-ui, -apple-system, sans-serif;
    z-index: 1000;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    border: 1px solid #ccc;
    display: none;
    max-height: 280px;
    width: 200px;
    flex-direction: column;
  }

  .boats-expanded .boats-panel {
    display: flex;
  }

  .boats-list {
    flex: 1;
    overflow-y: auto;
    padding: 6px 14px;
    padding-bottom: 0;
    max-height: 200px;
    -webkit-overflow-scrolling: touch; /* Smooth scrolling on iOS */
    border-bottom: 1px solid #ddd;
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
    margin: 10px 14px;
    background: #0066cc;
    color: white;
    border: none;
    border-radius: 4px;
    cursor: pointer;
    font-size: 12px;
    font-family: system-ui, -apple-system, sans-serif;
    flex-shrink: 0;
    -webkit-tap-highlight-color: transparent;
  }

  .fit-all-btn:hover {
    background: #0052a3;
  }

  .fit-all-btn:active {
    background: #004080;
  }

  /* Boats toggle button (bottom-right, 1rem gap from Layers) */
  .boats-toggle {
    position: absolute;
    bottom: 10px;
    right: calc(10px + 70px + 1rem); /* 10px margin + ~70px layers button + 1rem gap */
    padding: 6px 12px;
    background: rgba(255, 255, 255, 0.95);
    border: 1px solid #ccc;
    border-radius: 4px;
    cursor: pointer;
    font-size: 12px;
    font-family: system-ui, -apple-system, sans-serif;
    color: #333;
    box-shadow: 0 1px 4px rgba(0, 0, 0, 0.2);
    z-index: 1001;
  }

  .boats-toggle:hover {
    background: white;
    border-color: #999;
  }
</style>
