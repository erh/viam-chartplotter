<script lang="ts">
 import '../app.css'
 import { onMount } from 'svelte';

 import { Logger } from "tslog";
 
 import {Coordinate} from "tsgeo/Coordinate";
 import {DMS}        from "tsgeo/Formatter/Coordinate/DMS";

 import Collection from 'ol/Collection.js';
 import {useGeographic} from 'ol/proj.js';
 import Map from 'ol/Map';
 import View from 'ol/View';
 import TileLayer from 'ol/layer/Tile';
 import Point from 'ol/geom/Point.js';
 import TileWMS from 'ol/source/TileWMS.js';
 import Feature from 'ol/Feature.js';
 import VectorSource from 'ol/source/Vector.js';
 import {Vector, Tile} from 'ol/layer.js';

 import {
   Circle as CircleStyle,
   Fill,
   Icon,
   Stroke,
   Style,
 } from 'ol/style.js';


 import XYZ from 'ol/source/XYZ';

 import * as VIAM from '@viamrobotics/sdk';

 let gpsFormatter = new DMS();
 gpsFormatter.setSeparator("<br>")
             .useCardinalLetters(true)
             .setUnits(DMS.UNITS_ASCII);

 
 const globalLogger = new Logger({ name: "global" });
 let globalClient: VIAM.RobotClient;
 let globalClientLastReset = new Date();
 
 let globalData = {
   pos : new Coordinate(0,0),
   speed : 0.0,
   temp : 0.0,
   depth : 0.0,
   heading: 0.0,
   gauges : {},
   
   allResources : [],
   cameraNames : [],
   
   numUpdates: 0,
 };

 let status = "not connected yet";
 let map: Map = null;
 let view: View = null;
 let myBoatMarker: Feature = null;
 let allFeatures = new Collection();
 let lastData = new Date();
 
 let mapHelpers = {
   inPanMode: false,
   lastZoom: 0,
   lastCenter: null,
 };
 
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

 function gotNewData() {
   lastData = new Date();
 }
 
 function errorHandler(e) {
   globalLogger.error(e);
   var s = e.toString();
   status = "error: " + s;

   var reset = false;

   var diff = new Date() - lastData;

   if (diff > 1000 * 30) {
     reset = true;
   }
   
   if (s.indexOf("SESSION_EXPIRED") >= 0) {
     reset = true;
   }

   if (reset && (new Date() - globalClientLastReset) > 1000 * 30) {
     globalLogger.warn("Forcing reconnect b/c session_expired");
     status = "forcing reconnect b/c of error: " + e.toString();
     globalClient = null;
     globalClientLastReset = new Date();
   }

 }

 function pointDiff(x, y) {
   var a = x[0] - y[0];
   var b = x[1] - y[1];
   var c = a*a + b*b;
   return Math.sqrt(c);
 }

 function stopPanning() {
   mapHelpers.lastZoom = 0;
   mapHelpers.lastCenter = [0,0];
   mapHelpers.inPanMode = false;
 }
 
 function doUpdate(loopNumber: int, client: VIAM.RobotClient){
   const msClient = new VIAM.MovementSensorClient(client, 'cm90-garmin1-main:garmin');
   
   msClient.getPosition().then((p) => {
     mapHelpers.inGetPositionHelper = true;
     gotNewData();
     globalData.pos = new Coordinate(p.coordinate.latitude, p.coordinate.longitude);


     if (mapHelpers.lastZoom > 0 && mapHelpers.lastCenter != null && mapHelpers.lastCenter[0] != 0 ) {
       var z = view.getZoom();
       if (z != mapHelpers.lastZoom) {
         mapHelpers.inPanMode = true;
       }

       var c = view.getCenter();
       var diff = pointDiff(c, mapHelpers.lastCenter);
       if (diff > .003) {
         mapHelpers.inPanMode = true;
       }
     }

     
     if (!mapHelpers.inPanMode) {
       var sz = map.getSize();
       var pp = [globalData.pos.lng, globalData.pos.lat];
       view.centerOn(pp, map.getSize(), [sz[0]/2,sz[1]/2]);
       
       myBoatMarker.setGeometry(new Point(pp));
       
       // zoom of 10 is about 30 miles
       // zoom of 16 is city level
       var zoom = Math.floor(16-Math.sqrt(Math.floor(globalData.speed)^.5));
       view.setZoom(zoom);

       mapHelpers.lastZoom = zoom;
       mapHelpers.lastCenter = pp;
     }
     
   }).catch(errorHandler);

   msClient.getLinearVelocity().then((v) => {
     globalData.speed = v.y * 1.94384;
   }).catch(errorHandler);

   msClient.getCompassHeading().then((ch) => {
     globalData.heading = ch;
   }).catch(errorHandler);

   
   new VIAM.SensorClient(client, "cm90-garmin1-main:seatemp").getReadings().then((t) => {
     globalData.temp = 32 + (t.Temperature * 1.8);
   }).catch( errorHandler );

   new VIAM.SensorClient(client, "cm90-garmin1-main:depth-raw").getReadings().then((d) => {
     globalData.depth = d.Depth * 3.28084;
   }).catch( errorHandler );

   if (loopNumber % 30 == 2 ) {

     globalData.allResources.forEach( (r) => {
       if (r.subtype != "sensor") {
         return;
       }
       if (r.name.indexOf("fuel-") < 0 && r.name.indexOf("freshwater") < 0) {
         return;
       }
       
       var sp = r.name.split(":");
       var name = sp[sp.length-1];

       new VIAM.SensorClient(client, r.name).getReadings().then((raw) => {
         globalData.gauges[name] = raw;
       });
     });
       
     new VIAM.SensorClient(client, "cm90-garmin1-main:ais").getReadings().then((raw) => {
       var good = {};
       
       for ( var mmsi in raw  ) {
         
         var boat = raw[mmsi];
         
         if (boat == null || boat.Location == null || boat.Location.length != 2 || boat.Location[0] == null) {
           continue;
         }

         good[mmsi] = true;
         
         var found = false;

         for (var i = 1; i < allFeatures.getLength(); i++) {
           var v = allFeatures.item(i);
           if (v.get("mmsi") == mmsi) {
             found = true;
             v.setGeometry(new Point([boat.Location[1], boat.Location[0]]));
             break;
           }
         }
         
         if (found) {
           continue;
         }
         
         allFeatures.push(new Feature({
           type: "ais",
           mmsi: mmsi,
           heading: boat.Heading,
           geometry: new Point([boat.Location[1], boat.Location[0]]),
         }));
       }

       for (var i = 1; i < allFeatures.getLength(); i++) {
         var v = allFeatures.item(i);
         var mmsi = v.get("mmsi");
         if (!good[mmsi]) {
           allFeatures.removeAt(i);
         }
       }
       
     }).catch( errorHandler );
   }
   
   
 }

 function doCameraLoop(loopNumber: int, client: VIAM.RobotClient) {
   if (loopNumber % 10 > 0) {
     return;
   }

   globalData.allResources.forEach( (r) => {
     if (r.subtype != "camera") {
       return;
     }

     if (globalData.cameraNames.indexOf(r.name) < 0) {
       globalData.cameraNames.push(r.name);
       globalData.cameraNames.sort();
     }

     new VIAM.CameraClient(client, r.name).getImage().then(
       function(img){
         var i = document.getElementById(r.name);
         if (i) {
           i.src = URL.createObjectURL(new Blob([img]));
         }
     }).catch(errorHandler);
     
   });

 }

 async function updateResources(client: VIAM.RobotClient) {
   var resources = await client.resourceNames();
   globalData.allResources = resources;
 }
 
 async function updateAndLoop() {
   globalData.numUpdates++;
   
   if (!globalClient) {
     try {
       globalClient = await connect();
       await updateResources(globalClient);
       
     } catch(error) {
       status = "connect failed: " + error;
       globalClient = null;
     }
   }

   
   var client = globalClient;
   
   if (client) {
     doUpdate(globalData.numUpdates, client);
     doCameraLoop(globalData.numUpdates, client);
   }

   setTimeout(updateAndLoop, 1000);
 }

 async function connect(): VIAM.RobotClient {
   const urlParams = new URLSearchParams(window.location.search);
   
   const host = urlParams.get("host");
   const apiKey = urlParams.get("api-key");
   const authEntity = urlParams.get("authEntity");
   
   const credential = {
     type: 'api-key',
     payload: apiKey
   };

   var c = await VIAM.createRobotClient({
     host,
     credential: credential,
     authEntity: authEntity,
     signalingAddress: 'https://app.viam.com:443',

     // optional: configure reconnection options
     reconnectMaxAttempts: 20,
     reconnectMaxWait: 5000,
   });

   status = "connected";
   
   globalLogger.info('connected!');
   
   c.on('disconnected', disconnected);
   c.on('reconnected', reconnected);

   return c;
 }

 async function disconnected(event) {
   status = "disconnected";
   globalLogger.warn('The robot has been disconnected. Trying reconnect...');
 }

 async function reconnected(event) {
   status = "connected";
   globalLogger.warn('The robot has been reconnected. Work can be continued.');
 }


 async function start() {

   try {
     setupMap();
     updateAndLoop();
     return {}     
   } catch (error) {
     errorHandler(error);
   }
 }

 function setupMap() {
   

   useGeographic();
   
   view = new View({
     center: [0, 0],
     zoom: 15
   });

   var layers = [];

   // core open stream maps
   if (true) {
     layers.push(new TileLayer({
       opacity: .5,
       source: new XYZ({
         url: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png'
       })
     }));
   }
   
   // depth data
   if (true) {
     layers.push(new TileLayer({
       opacity: .7,
       source: new TileWMS({
         url: 'https://geoserver.openseamap.org/geoserver/gwc/service/wms',
         params: {'LAYERS': 'gebco2021:gebco_2021', 'VERSION':'1.1.1'},
         ratio: 1,
         serverType: 'geoserver',
         hidpi: false,
       }),
     }));
   }
   
   // harbors
   if (true) {
     var layer_seamark = new TileLayer({
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
     });
     layers.push(layer_seamark);
   }

   if (false) {
     layers.push(new TileLayer({
       opacity: .7,
       source: new TileWMS({
         url: "https://gis.charttools.noaa.gov/arcgis/rest/services/MCS/NOAAChartDisplay/MapServer/exts/MaritimeChartService/WMSServer",
         //params: {'LAYERS': 'gebco2021:gebco_2021', 'VERSION':'1.1.1'},
         //ratio: 1,
         //serverType: 'geoserver',
         //hidpi: false,
       }),
     }));
   }

   myBoatMarker = new Feature({
     type: 'geoMarker',
     header: 0,
     geometry: new Point([0,0]),
   });

   allFeatures.push(myBoatMarker);
   
   var vectorLayer = new Vector({
     source: new VectorSource({
       features: allFeatures,
     }),
     style: function (feature) {

       var scale = 0.5;
       var rotation = 0;
       
       if (feature.get("type") == "ais") {
         scale = 0.25;
         var h = feature.get("heading");
         if (h >= 0 && h < 360) {
           rotation = (h/ 360) * Math.PI * 2;
         }
       } else {
         rotation = (globalData.heading / 360) * Math.PI * 2;
       }
       
       return new Style({
         image: new Icon(
           {
             src:"/boat3.jpg",
             scale: scale,
             rotation: rotation,
         }),
       });
     },
   });
   layers.push(vectorLayer);
   
   map = new Map({
     target: 'map',
     layers: layers,
     view: view
   });
 }

 onMount(start);

 function gaugesToArray(gauges) {
   var names = Object.keys(gauges);
   names.sort();

   var a = [];
   
   for ( var i = 0; i < names.length; i++) {
     var n = names[i];
     a.push( [ n, gauges[n] ]);
   }
   return a;
 }
</script>


<div>
  <table border="1">
    <tr>
      <th colspan="2">{status}</th>
    </tr>
    <tr>
      <td>
        <div id="map"></div>
        {#if mapHelpers.inPanMode}
          <button on:click="{stopPanning}">Stop Panning</button>
        {/if}
      </td>
      <td id="navData">
        <div class="data" >
          <div>Speed knots</div>
          {globalData.speed.toFixed(2)}
        </div>
        <div class="data" >
          <div>Depth ft</div>
          {globalData.depth.toFixed(2)}
        </div>
        <div class="data" >
          <div>Water Temp (f)</div>
          {globalData.temp.toFixed(2)} f
        </div>
        <div class="data" >
          <div>Location</div>
          {@html globalData.pos.format(gpsFormatter)}
        </div>
        <div class="data" >
          <div>Heading</div>
          {@html globalData.heading}
        </div>
        <table class="gauge">
          {#each gaugesToArray(globalData.gauges) as [key, value]}
            <tr>
              <th>{key}</th>
              <td>{value.Level} %</td>
              <td>{(value.Capacity * value.Level * 0.264172 / 100).toFixed(0)} g</td>
            </tr>
          {/each}
        </table>
      </td>
    </tr>
    <tr>
      <td colspan="2">
        {#each globalData.cameraNames as name, index}
          <img id="{name}" width="250" alt="{name}" />
        {/each}
      </td>
    </tr>
  </table>
</div>
        
<small>{globalData.numUpdates}</small>
