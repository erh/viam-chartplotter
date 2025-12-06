<script lang="ts">

 import { onMount } from 'svelte';
import type { BoatInfo } from './lib/BoatInfo';
 
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
 import Circle from 'ol/geom/Circle.js';
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
   on: boolean;
   layer: TileLayer<any> | Vector<any>;
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
   },
 });

 let layersExpanded = $state(false);
 let boatsExpanded = $state(false);

 // Track which boats are visible (by mmsi, plus 'myBoat' for own boat)
 let visibleBoats = $state<Set<string>>(new Set(['myBoat']));

 // Initialize visibleBoats when boats prop changes
 let lastBoatsLength = $state(0);
 $effect(() => {
   const currentLength = boats?.length ?? 0;
   if (currentLength !== lastBoatsLength) {
     // Add any new boats to visible set
     boats?.forEach(b => {
       if (b.mmsi && !visibleBoats.has(b.mmsi)) {
         visibleBoats.add(b.mmsi);
       }
     });
     lastBoatsLength = currentLength;
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

 function fitToVisibleBoats() {
   if (!mapGlobal.map || !mapGlobal.view) return;

   const coords: number[][] = [];

   // Add my boat if visible
   if (visibleBoats.has('myBoat') && (myBoat.location[0] !== 0 || myBoat.location[1] !== 0)) {
     coords.push([myBoat.location[1], myBoat.location[0]]); // [lng, lat]
   }

   // Add visible AIS boats
   boats?.forEach(boat => {
     if (boat.mmsi && visibleBoats.has(boat.mmsi)) {
       coords.push([boat.location[1], boat.location[0]]);
     }
   });

   if (coords.length === 0) return;

   if (coords.length === 1) {
     // Single boat - center on it with reasonable zoom
     mapGlobal.view.animate({
       center: coords[0],
       zoom: 10,
       duration: 500
     });
   } else {
     // Multiple boats - fit to extent with generous padding for popups
     const extent = boundingExtent(coords);
     mapGlobal.view.fit(extent, {
       padding: [180, 80, 80, 80], // Extra top padding for popups
       duration: 500,
       maxZoom: 12
     });
   }

   mapInternalState.inPanMode = true; // Prevent auto-centering
 }

 let { myBoat, zoomModifier, boats, positionHistorical, enableBoatsPanel = false }: {
  myBoat: BoatInfo;
  zoomModifier?: number;
  boats?: BoatInfo[];
  positionHistorical?: { lat: number; lng: number }[];
  enableBoatsPanel?: boolean;
} = $props();

 // Create derived values for reactivity tracking
 let boatsKey = $derived(JSON.stringify(boats?.map(b => [b.location, b.speed, b.heading])));
 let myBoatKey = $derived(JSON.stringify([myBoat.heading, myBoat.location, myBoat.speed, myBoat.route]));
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
   
   if (popupState.content.isMyBoat) {
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

 $effect(() => {
   mapGlobal.layerOptions.forEach((l) => l.on);
   updateOnLayers();
 });

 let mapGlobal = $state({

   map: null as Map | null,
   view: null as View | null,

   aisFeatures: new Collection<Feature<Geometry>>(),
   trackFeatures: new Collection<Feature<Geometry>>(),
   routeFeatures: new Collection<Feature<Geometry>>(),
   trackFeaturesLastCheck : new Date(0),
   myBoatMarker: null as Feature<Point> | null,
   

   layerOptions: [] as LayerOption[],
   onLayers: new Collection<BaseLayer>(),
 });

 let mapInternalState: {
   inPanMode: boolean;
   lastZoom: number;
   lastCenter: number[] | null;
   lastPosition: number[];
   trackFeatureIds: Record<string, boolean>;
 } = {
   inPanMode: false,
   lastZoom: 0,
   lastCenter: null,
   lastPosition: [0,0],
   trackFeatureIds: {},
 }

 function updateFromData() {
   if (!mapGlobal.map || !mapGlobal.view || !mapGlobal.myBoatMarker) {
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
     var addToTrack = false;
     if (mapInternalState.lastPosition[0] == 0) {
       addToTrack = true;
     } else {
       var diff = pointDiff(mapInternalState.lastPosition, pp);
       if (diff > .0000001) {
         addToTrack = true;
       }
     }
     if (addToTrack) {
       mapGlobal.trackFeatures.push(new Feature({
         type: "track",
         geometry: new Circle(pp),
       }));
     }
     
     mapInternalState.lastPosition = pp;
   }
   
   // route stuff
   mapGlobal.routeFeatures.clear();
   if (myBoat.route && myBoat.route.destinationLongitude && myBoat.route.destinationLatitude) {
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
       
       for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
         var v = mapGlobal.aisFeatures.item(i) as Feature<Geometry>;
         if (v.get("mmsi") == mmsi) {
           v.setGeometry(new Point([boat.location[1], boat.location[0]]));
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
         geometry: new Point([boat.location[1], boat.location[0]]),
       }));
     });

     for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
       var v = mapGlobal.aisFeatures.item(i) as Feature<Geometry>;
       var mmsi = v.get("mmsi") as string;
       if (!seen[mmsi]) {
         mapGlobal.aisFeatures.removeAt(i);
       }
     }
   }

   if (positionHistorical) {
     var prev: number[] | null = null;
     positionHistorical.forEach( (p) => {
       var pp = [p.lng, p.lat];

       addTrackFeature("p-" + p.lng + "-" + p.lat,
                        new Circle(pp));
       
       if (prev) {
         addTrackFeature("line-" + p.lng + "-" + p.lat, 
                          new LineString([prev, pp]));
       }
       prev = pp;
     });
   }

 }

 function addTrackFeature(id: string, g: Geometry) {
   if (mapInternalState.trackFeatureIds[id] == true) {
     return;
   }

   mapInternalState.trackFeatureIds[id] = true;
   
   mapGlobal.trackFeatures.push(new Feature({
     type: "track",
     "myid" : id,
     geometry: g,
   }));
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
       source: new XYZ({
         url: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png'
       })
     }),
   })
   
   // depth data
   mapGlobal.layerOptions.push({
     name: "depth",
     on: false,
     layer: new TileLayer({
       opacity: .7,
       source: new TileWMS({
         url: 'https://geoserver.openseamap.org/geoserver/gwc/service/wms',
         params: {'LAYERS': 'gebco2021:gebco_2021', 'VERSION':'1.1.1'},
         serverType: 'geoserver',
         hidpi: false,
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
       source: new XYZ({
         tileUrlFunction: function(coordinate) {
           return getTileUrlFunction("https://tiles.openseamap.org/seamark/", 'png', coordinate);
   }
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
       source: new TileWMS({
         url: "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer",
         params: {},
         //ratio: 1,
         //serverType: 'geoserver',
         //hidpi: false,
       }),
     }),
   })

   // by boat setup
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
       // Hide if myBoat is not in visibleBoats
       if (!visibleBoats.has('myBoat')) {
         return new Style({}); // Empty style = hidden
       }
       
       var scale = 0.6;
       var rotation = (myBoat.heading / 360) * Math.PI * 2;
       
       return new Style({
         image: new Icon(
           {
             src:boatImage,
             scale: scale,
             rotation: rotation,
         }),
       });
     },
   });
   mapGlobal.layerOptions.push({
     name: "boat",
     on: true,
     layer : myBoatLayer,
   });
   
   var aisLayer = new Vector({
     source: new VectorSource({
       features: mapGlobal.aisFeatures,
     }),
     style: function (feature) {
       // Hide if boat is not visible
       if (!feature.get("visible")) {
         return new Style({}); // Empty style = hidden
       }

       var scale = 0.25;
       var rotation = 0;
       
       var h = feature.get("heading");
       if (h >= 0 && h < 360) {
         rotation = (h/ 360) * Math.PI * 2;
       }
       
       return new Style({
         image: new Icon(
           {
             src:boatImage,
             scale: scale,
             rotation: rotation,
         }),
       });
     },
   });

   mapGlobal.layerOptions.push({
     name: "ais",
     on: true,
     layer : aisLayer,
   });

   var trackLayer = new Vector({
     source: new VectorSource({
       features: mapGlobal.trackFeatures,
     }),
     style: new Style({
       stroke: new Stroke({
         color: "blue",
         width: 3
       }),
       fill: new Fill({
         color: "rgba(0, 255, 0, 0.1)"
       })
     }),
   });

   mapGlobal.layerOptions.push({
     name: "track",
     on: true,
     layer : trackLayer,
   });
   
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
     if (l.on) {
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
   mapGlobal.map.on("click", function (evt) {
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

       if (type === "geoMarker") {
         popupState.content = {
           name: "My Boat",
           mmsi: "",
           speed: myBoat.speed,
           heading: myBoat.heading,
           lat: coords[1],
           lng: coords[0],
           isMyBoat: true,
         };
       } else {
         popupState.content = {
           name: feature.get("name") || "Unknown",
           mmsi: feature.get("mmsi") || "",
           speed: feature.get("speed") || 0,
           heading: feature.get("heading") || 0,
           lat: coords[1],
           lng: coords[0],
           isMyBoat: false,
         };
       }
       popupState.visible = true;
       popupState.overlay?.setPosition(coords);
     } else {
       closePopup();
     }
   });

   // Change cursor on hover over boats
   mapGlobal.map.on("pointermove", function (evt) {
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
   });

   console.log("setupMap finished");
   
   // Initial fit to show all boats with room for popups (only when boats panel enabled)
   setTimeout(() => {
     if (enableBoatsPanel && boats && boats.length > 0) {
       fitToVisibleBoats();
     }
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
   };
 });

</script>

<div id="map-container" class="relative lg:col-span-3 row-span-3 lg:row-span-5 border border-dark" class:layers-expanded={layersExpanded} class:boats-expanded={boatsExpanded}>
  <div id="map" class="w-full aspect-video bg-white"></div>

  <!-- Boat Info Popup -->
  <div id="boat-popup" class="boat-popup" class:hidden={!popupState.visible}>
    <button class="popup-closer" onclick={closePopup}>✕</button>
    <div class="popup-content">
      <h3 class="popup-title">{popupState.content.name}</h3>
      {#if !popupState.content.isMyBoat && popupState.content.mmsi}
        <div class="popup-row">
          <span class="popup-label">MMSI</span>
          <span class="popup-value">{popupState.content.mmsi}</span>
        </div>
      {/if}
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
    <div class="popup-arrow"></div>
  </div>

  <div class="layer-controls">
    {#if mapInternalState.inPanMode}
      <div>
        <button onclick={stopPanning}>Stop Panning</button>
      </div>
    {/if}
    {#each mapGlobal.layerOptions as l, idx}
      <label>
        <input type="checkbox" bind:checked={mapGlobal.layerOptions[idx].on}>
        {l.name}
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
    <div class="boats-list">
      <label class="boat-item">
        <input 
          type="checkbox" 
          checked={visibleBoats.has('myBoat')}
          onchange={() => toggleBoatVisibility('myBoat')}
        >
        <span class="boat-name my-boat">My Boat</span>
      </label>
      {#if boats}
        {#each boats as boat}
          {#if boat.mmsi}
            <label class="boat-item">
              <input 
                type="checkbox" 
                checked={visibleBoats.has(boat.mmsi)}
                onchange={() => toggleBoatVisibility(boat.mmsi!)}
              >
              <span class="boat-name">{boat.name}</span>
            </label>
          {/if}
        {/each}
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
  .boat-popup {
    position: absolute;
    background: rgba(15, 23, 42, 0.95);
    backdrop-filter: blur(8px);
    color: white;
    border-radius: 4px;
    padding: 10px 12px;
    min-width: 160px;
    box-shadow: 0 4px 20px rgba(0, 0, 0, 0.5);
    border: 1px solid rgba(255, 255, 255, 0.08);
    font-family:
      system-ui,
      -apple-system,
      sans-serif;
    z-index: 1000;
    transform: translate(-50%, -100%);
    margin-bottom: 50px;
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

  .popup-content {
    display: flex;
    flex-direction: column;
    gap: 3px;
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
    gap: 16px;
  }

  .popup-label {
    color: rgba(255, 255, 255, 0.5);
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    min-width: 28px;
  }

  .popup-value {
    font-weight: 500;
    font-size: 12px;
    text-align: right;
    font-variant-numeric: tabular-nums;
    font-family: ui-monospace, monospace;
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
    padding: 10px 14px;
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
