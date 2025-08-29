<script lang="ts">

 import { onMount } from 'svelte';
 
 import Collection from 'ol/Collection.js';
 import {useGeographic} from 'ol/proj.js';
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

 let boatImage = "boat3.jpg";

 let { position, speed, heading, zoomModifier, route, boats, positionHistorical} = $props();

 $effect( () => {
   if (heading || position || speed || route) {
     updateFromData();
   }
 });

 let mapGlobal = $state({

   map: null,
   view: null,

   aisFeatures: new Collection(),
   trackFeatures: new Collection(),
   routeFeatures: new Collection(),
   trackFeaturesLastCheck : new Date(0),
   myBoatMarker: null,
   

   layerOptions: [],
   onLayers: new Collection(),
 });

 let mapInternalState = {
   inPanMode: false,
   lastZoom: 0,
   lastCenter: null,
   lastPosition: [0,0],
 }

 function updateFromData() {
   if (!mapGlobal.map) {
     return
   }

   if (mapInternalState.lastZoom > 0 && mapInternalState.lastCenter != null && mapInternalState.lastCenter[0] != 0 ) {
     var z = mapGlobal.view.getZoom();
     if (z != mapInternalState.lastZoom) {
       mapInternalState.inPanMode = true;
     }
     
     var c = mapGlobal.view.getCenter();
     var diff = pointDiff(c, mapInternalState.lastCenter);
     if (diff > .003) {
       mapInternalState.inPanMode = true;
     }
   }
   
   var sz = mapGlobal.map.getSize();
   var pp = [position.lng, position.lat];
   mapGlobal.myBoatMarker.setGeometry(new Point(pp));
   
   if (!mapInternalState.inPanMode) {
     mapGlobal.view.centerOn(pp, mapGlobal.map.getSize(), [sz[0]/2,sz[1]/2]);
     
     // zoom of 10 is about 30 miles
     // zoom of 16 is city level
     var zoom = Math.pow(Math.floor(speed),.41)
     zoom = Math.floor(16-zoom) + (zoomModifier||0);
     if ( zoom <= 0 ) {
       zoom = 1;
     }
     //console.log("speed: " + speed + " zoom: " + zoom);
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
   if (route) {
     var prev = [];
     for (var x=0; x<route.length; x++) {
       var wp = route[x];
       var loc = [wp["WP Longitude"], wp["WP Latitude"]];
       if (prev) {
         var f = new Feature({
           type: "track",
           geometry: new LineString([prev, loc]),
         });
         mapGlobal.routeFeatures.push(f);
       }
       prev = loc;
     }
   }

   if (boats == null) {
     mapGlobal.aisFeatures.Clear();
   } else {
     var seen = {};
     boats.forEach( (boat) => {

       var mmsi = boat["User ID"];
       if (!mmsi) {
         return;
       }
       seen[mmsi] = true;
       
       for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
         var v = mapGlobal.aisFeatures.item(i);
         if (v.get("mmsi") == mmsi) {
           v.setGeometry(new Point([boat.Location[1], boat.Location[0]]));
           return;
         }
       }

       mapGlobal.aisFeatures.push(new Feature({
         type: "ais",
         name: boat.Name,
         mmsi: mmsi,
         heading: boat.Heading,
         geometry: new Point([boat.Location[1], boat.Location[0]]),
       }));
     });

     for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
       var v = mapGlobal.aisFeatures.item(i);
       var mmsi = v.get("mmsi");
       if (!seen[mmsi]) {
         mapGlobal.aisFeatures.removeAt(i);
       }
     }
   }

   if (positionHistorical) {
     var prev = null;
     positionHistorical.forEach( (p) => {
       var pp = [p.lng, p.lat];

       addTreackFeature("p-" + p.lng + "-" + p.lat,
                        new Circle(pp));
       
       if (prev) {
         addTreackFeature("line-" + p.lng + "-" + p.lat, 
                          new LineString([prev, pp]));
       }
       prev = pp;
     });
   }

 }

 function addTreackFeature(id, g) {
   for (var i = 0; i < mapGlobal.trackFeatures.getLength(); i++) {
     var v = mapGlobal.trackFeatures.item(i);
     if (id == v.get("myid")) {
       return;
     }
   }
       
   mapGlobal.trackFeatures.push(new Feature({
     type: "track",
     "myid" : id,
     geometry: g,
   }));
 }
 
 function getTileUrlFunction(url, type, coordinates) {
   var x = coordinates[1];
   var y = coordinates[2];
   var z = coordinates[0];
   var limit = Math.pow(2, z);
   if (y < 0 || y >= limit) {
     return null;
   } else {
     x = ((x % limit) + limit) % limit;
     
     var path = z + "/" + x + "/" + y + "." + type;
     if (url instanceof Array) {
       url = this.selectUrl(path, url);
     }
     return url + path;
   }
 }

 function stopPanning() {
   mapInternalState.lastZoom = 0;
   mapInternalState.lastCenter = [0,0];
   mapGlobal.inPanMode = false;
 }

 function setupLayers() {

   // core open street maps
   mapGlobal.layerOptions.push( {
     name : "open street map",
     on : false,
     layer : new TileLayer({
       opacity: .5,
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
         ratio: 1,
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
       maxZom: 19,
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
     on: true,
     layer: new TileLayer({
       opacity: .7,
       source: new TileWMS({
         url: "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer",
         //params: {'LAYERS': 'gebco2021:gebco_2021', 'VERSION':'1.1.1'},
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
   
   var myBoatFeatures = new Collection();
   myBoatFeatures.push(mapGlobal.myBoatMarker);

   var myBoatLayer = new Vector({
     source: new VectorSource({
       features: myBoatFeatures,
     }),
     style: function (feature) {
       
       var scale = 0.6;
       var rotation = (heading / 360) * Math.PI * 2;
       
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

 function findLayerByName(name) {
   for( var l of mapGlobal.layerOptions) {
     if (l.name == name) {
       return l;
     }
   }
   return null;
 }

 function findOnLayerIndexOfName(name) {
   var l = findLayerByName(name);
   if (l == null) {
     return -2;
   }

   for ( var i=0; i<mapGlobal.onLayers.getLength(); i++) {
     if (mapGlobal.onLayers.item(i).ol_uid == l.layer.ol_uid) {
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
         mapGlobal.onLayers.push(l.layer);
       }
     } else {
       if ( idx >= 0 ) {
         mapGlobal.onLayers.removeAt(idx);
       }
     }
   }
 }
 
 function pointDiff(x, y) {
   var a = x[0] - y[0];
   var b = x[1] - y[1];
   var c = a*a + b*b;
   return Math.sqrt(c);
 }

 function setupMap() {
   const urlParams = new URLSearchParams(window.location.search);
   var temp = urlParams.get("zoomModifier");
   if (temp) {
     temp = parseInt(temp);
     globalConfig.zoomModifier = temp;
   }
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
     layers: mapGlobal.onLayers,
     view: mapGlobal.view,
     controls: defaultControls().extend([scaleThing])
   });

   console.log("setupMap finished");
 }

 onMount(setupMap);

</script>

<div id="map-container" class="relative lg:col-span-3 row-span-3 lg:row-span-5 border border-dark">
  <div id="map" class="min-h-[50dvh] h-fit bg-white"></div>
  <div class="absolute bottom-0 right-0">
    {#if mapInternalState.inPanMode}
      <div>
        <button on:click="{stopPanning}">Stop Panning</button>
      </div>
    {/if}
    {#each mapGlobal.layerOptions as l, idx}
      <div>
        <input type="checkbox" bind:checked={mapGlobal.layerOptions[idx].on}>
        {l.name}
      </div>
    {/each}
  </div>
</div>
