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
   LinkedChart,
   LinkedLabel,
   LinkedValue
 } from "svelte-tiny-linked-charts"

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

 let globalCloudClient: VIAM.ViamClient;
 
 let globalData = {
   pos : new Coordinate(0,0),
   speed : 0.0,
   temp : 0.0,
   depth : 0.0,
   heading: 0.0,
   gauges : {},
   gaugesToHistorical : {},
   
   allResources : [],

   cameraNames : [],
   lastCameraTimes : [],
   
   numUpdates: 0,
   status: "not connected yet",
   lastData: new Date(),
   
 };

 var globalConfig = {
   movementSensorName : "",
   movementSensorProps : {},
   
   aisSensorName : "",
   seatempSensorName : "",
   depthSensorName : "",
 };
 
 let mapGlobal = {

   map: null,
   view: null,

   aisFeatures: new Collection(),
   myBoatMarker: null,
   
   inPanMode: false,
   lastZoom: 0,
   lastCenter: null,

   layerOptions: [],
   onLayers: new Collection(),
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
   globalData.lastData = new Date();
 }
 
 function errorHandler(e) {
   globalLogger.error(e);
   var s = e.toString();
   globalData.status = "error: " + s;

   var reset = false;

   var diff = new Date() - globalData.lastData;

   if (diff > 1000 * 30) {
     reset = true;
   }
   
   if (s.indexOf("SESSION_EXPIRED") >= 0) {
     reset = true;
   }

   if (reset && (new Date() - globalClientLastReset) > 1000 * 30) {
     globalLogger.warn("Forcing reconnect b/c session_expired");
     globalData.status = "forcing reconnect b/c of error: " + e.toString();
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
   mapGlobal.lastZoom = 0;
   mapGlobal.lastCenter = [0,0];
   mapGlobal.inPanMode = false;
 }
 
 function doUpdate(loopNumber: int, client: VIAM.RobotClient){
   const msClient = new VIAM.MovementSensorClient(client, globalConfig.movementSensorName);
   
   msClient.getPosition().then((p) => {
     mapGlobal.inGetPositionHelper = true;
     gotNewData();
     globalData.pos = new Coordinate(p.coordinate.latitude, p.coordinate.longitude);


     if (mapGlobal.lastZoom > 0 && mapGlobal.lastCenter != null && mapGlobal.lastCenter[0] != 0 ) {
       var z = mapGlobal.view.getZoom();
       if (z != mapGlobal.lastZoom) {
         mapGlobal.inPanMode = true;
       }

       var c = mapGlobal.view.getCenter();
       var diff = pointDiff(c, mapGlobal.lastCenter);
       if (diff > .003) {
         mapGlobal.inPanMode = true;
       }
     }

     
     if (!mapGlobal.inPanMode) {
       var sz = mapGlobal.map.getSize();
       var pp = [globalData.pos.lng, globalData.pos.lat];
       mapGlobal.view.centerOn(pp, mapGlobal.map.getSize(), [sz[0]/2,sz[1]/2]);
       
       mapGlobal.myBoatMarker.setGeometry(new Point(pp));
       
       // zoom of 10 is about 30 miles
       // zoom of 16 is city level
       var zoom = Math.floor(16-Math.sqrt(Math.floor(globalData.speed)^.5));
       mapGlobal.view.setZoom(zoom);

       mapGlobal.lastZoom = zoom;
       mapGlobal.lastCenter = pp;
     }
     
   }).catch(errorHandler);

   msClient.getLinearVelocity().then((v) => {
     globalData.speed = v.y * 1.94384;
   }).catch(errorHandler);

   msClient.getCompassHeading().then((ch) => {
     globalData.heading = ch;
   }).catch(errorHandler);


   if (globalConfig.seatempSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.seatempSensorName).getReadings().then((t) => {
       globalData.temp = 32 + (t.Temperature * 1.8);
     }).catch( errorHandler );
   }

   if (globalConfig.depthSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.depthSensorName).getReadings().then((d) => {
       globalData.depth = d.Depth * 3.28084;
     }).catch( (e) => {
       globalConfig.depthSensorName = "";
       errorHandler(e);
     });
   }
   
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

     if (globalConfig.aisSensorName != "") {
       new VIAM.SensorClient(client, globalConfig.aisSensorName).getReadings().then((raw) => {
         var good = {};
         
         for ( var mmsi in raw  ) {
           
           var boat = raw[mmsi];
           
           if (boat == null || boat.Location == null || boat.Location.length != 2 || boat.Location[0] == null) {
             continue;
           }
           
           good[mmsi] = true;
           
           var found = false;
           
           for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
             var v = mapGlobal.aisFeatures.item(i);
             if (v.get("mmsi") == mmsi) {
               found = true;
               v.setGeometry(new Point([boat.Location[1], boat.Location[0]]));
               break;
             }
           }
           
           if (found) {
             continue;
           }
           
           mapGlobal.aisFeatures.push(new Feature({
             type: "ais",
             mmsi: mmsi,
             heading: boat.Heading,
             geometry: new Point([boat.Location[1], boat.Location[0]]),
           }));
         }
         
         for (var i = 0; i < mapGlobal.aisFeatures.getLength(); i++) {
           var v = mapGlobal.aisFeatures.item(i);
           var mmsi = v.get("mmsi");
           if (!good[mmsi]) {
             mapGlobal.aisFeatures.removeAt(i);
           }
         }
         
       }).catch( errorHandler );
     }
   }
   
   
 }

 function doCameraLoop(loopNumber: int, client: VIAM.RobotClient) {

   while (globalData.lastCameraTimes.length > 20){
     globalData.lastCameraTimes.shift();
   }

   if (globalData.lastCameraTimes.length > 0) {
     var avg = globalData.lastCameraTimes.reduce( (a,b) => a + b) / globalData.lastCameraTimes.length;
     var mod = Math.floor((avg * 20) / 1000);
     
     if (mod > 0 && loopNumber > 4 && loopNumber % mod > 0) {
       return;
     }
     
   }

   var start = new Date();
   
   filterResources(globalData.allResources, "component", "camera").forEach( (r) => {
     if (globalData.cameraNames.indexOf(r.name) < 0) {
       globalData.cameraNames.push(r.name);
       globalData.cameraNames.sort();
     }

     new VIAM.CameraClient(client, r.name).getImage().then(
       function(img){
         var ms = (new Date()) - start;
         globalData.lastCameraTimes.push(ms);
         var i = document.getElementById(r.name);
         if (i) {
           i.src = URL.createObjectURL(new Blob([img]));
         }
     }).catch(errorHandler);
     
   });

 }

 // t - type
 // st - subtype
 // n - name regex
 function filterResources(resources, t, st, n) {
   var a = [];
   for (var r of resources) {
     if (t != "", r.type != t) {
       continue;
     }

     if (st != "", r.subtype != st) {
       continue;
     }

     if (n != null && !r.name.match(n) ) {
       continue;
     }

     a.push(r);
   }

   return a;
 }
 
 async function updateResources(client: VIAM.RobotClient) {
   var resources = await client.resourceNames();
   globalData.allResources = resources;

   await setupMovementSensor(client, resources);
   await setupAISSensor(client, resources);
   await setupTempSensor(client, resources);
   await setupDepthSensor(client, resources);

   console.log("globalConfig", globalConfig);

 }
 
 async function setupAISSensor(client: VIAM.RobotClient, resources) {
   resources = filterResources(resources, "component", "sensor", /\bais$/);

   for (var r of resources) {
     globalConfig.aisSensorName = r.name;
   }

 }
 
 async function setupTempSensor(client: VIAM.RobotClient, resources) {
   resources = filterResources(resources, "component", "sensor", /\bseatemp$/);

   for (var r of resources) {
     globalConfig.seatempSensorName = r.name;
   }

 }
 
 async function setupDepthSensor(client: VIAM.RobotClient, resources) {
   resources = filterResources(resources, "component", "sensor", /depth/);

   for (var r of resources) {
     globalConfig.depthSensorName = r.name;
   }

 }

 
 async function setupMovementSensor(client: VIAM.RobotClient, resources) {
   resources = filterResources(resources, "component", "movement_sensor", null);
   
   // pick best movement sensor
   var bestName = "";
   var bestScore = 0;
   var bestProp = {};
   
   for (var r of resources) {
     const msClient = new VIAM.MovementSensorClient(client, r.name);
     var prop = await msClient.getProperties();

     var score = 0;
     if (prop.positionSupported) {
       score++;
     }
     if (prop.linearVelocitySupported) {
       score++;
     }
     if (prop.compassHeadingSupported) {
       score++;
     }

     if (score > bestScore) {
       bestName = r.name;
       bestScore = score;
       bestProp = prop;
     }
   }
   
   globalConfig.movementSensorName = bestName;
   globalConfig.movementSensorProps = bestProp;
 }
 
 async function updateAndLoop() {
   globalData.numUpdates++;
   
   if (!globalClient) {
     try {
       globalClient = await connect();
       await updateResources(globalClient);

     } catch(error) {
       globalData.status = "connect failed: " + error;
       globalClient = null;
     }
   } else if (globalClient.numUpdates % 300 == 0) {
     await updateResources(globalClient);     
   }

   updateOnLayers();
   
   var client = globalClient;
   
   if (client) {
     doUpdate(globalData.numUpdates, client);
     doCameraLoop(globalData.numUpdates, client);
   }

   setTimeout(updateAndLoop, 1000);
   if (globalData.numUpdates == 1) {
     setTimeout(updateCloudDataAndLoop, 1000);
   }
 }

 async function updateCloudDataAndLoop() {
   const urlParams = new URLSearchParams(window.location.search);
   
   if (!globalCloudClient) {
     try {
       
       const opts: VIAM.ViamClientOptions = {
         credential: {
           type: 'api-key',
           authEntity: urlParams.get("authEntity"),
           payload: urlParams.get("api-key"),
         },
       };
       
       globalCloudClient = await VIAM.createViamClient(opts);
       
     } catch( error ) {
       console.log("cannot connect to cloud: " + error);
     }
   }
   
   if (globalCloudClient) {
     var dc = globalCloudClient.dataClient;

     var hostPieces = urlParams.get("host").split("."); // TODO - fix
     var robotName = hostPieces[0].split("-main")[0]; // TODO - fix
     var location = hostPieces[1]; // TODO - fix

     updateGaugeGraphs(globalCloudClient.dataClient, robotName, location);
   }

   setTimeout(updateCloudDataAndLoop, 1000);
 }

 async function updateGaugeGraphs(dc, robotName, location) {
   for ( var g in globalData.gauges ) {
     var h = globalData.gaugesToHistorical[g];
     if (h && (new Date() - h.ts) < 60000) {
       continue;
     }

     var f = dc.createFilter({
       robotName: robotName,
       locationIdsList: [location],
       startTime: new Date(new Date() - 86400 * 1000),
       componentName: g,
     });

     var data = await dc.tabularDataByFilter(f);

     h = { ts : new Date(), data : data };
     globalData.gaugesToHistorical[g] = h;
     
   }
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

   globalData.status = "connected";
   
   globalLogger.info('connected!');
   
   c.on('disconnected', disconnected);
   c.on('reconnected', reconnected);

   return c;
 }

 async function disconnected(event) {
   globalData.status = "disconnected";
   globalLogger.warn('The robot has been disconnected. Trying reconnect...');
 }

 async function reconnected(event) {
   globalData.status = "connected";
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

 function setupLayers() {

   // core open street maps
   mapGlobal.layerOptions.push( {
     name : "open street map",
     on : true,
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
     on: true,
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
     on: true,
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
     on: false,
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
       var rotation = (globalData.heading / 360) * Math.PI * 2;
       
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
             src:"/boat3.jpg",
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
 
 function setupMap() {
   useGeographic();
   setupLayers();
   
   mapGlobal.view = new View({
     center: [0, 0],
     zoom: 15
   });

   updateOnLayers();
   updateOnLayers();
   
   mapGlobal.map = new Map({
     target: 'map',
     layers: mapGlobal.onLayers,
     view: mapGlobal.view
   });
 }

 onMount(start);

 function formatDate(date) {
   return (date.getMonth()+1) + "/" + date.getDate() + "-" + date.getHours() + ":" + date.getMinutes();
 }

 
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

 function gauageHistoricalToLinkedChart(data) {
   var res = {};
   for (var d in data.data) {
     var dd = data.data[d];
     res[formatDate(dd.timeReceived)] = dd.data.readings.Level;
   }
   return res;
 }
</script>


<div>
  <table border="1">
    <tr>
      <th colspan="2">{globalData.status}</th>
    </tr>
    <tr>
      <td>
        <div id="map"></div>
        {#if mapGlobal.inPanMode}
          <button on:click="{stopPanning}">Stop Panning</button>
        {/if}
        {#each mapGlobal.layerOptions as l, idx}
          <input type="checkbox" bind:checked={mapGlobal.layerOptions[idx].on}>
          {l.name}
        {/each}
      </td>
      <td id="navData">
        {#if globalConfig.movementSensorProps.linearVelocitySupported}
          <div class="data" >
            <div>Speed knots</div>
            {globalData.speed.toFixed(2)}
          </div>
        {/if}
        {#if globalConfig.depthSensorName != ""}
          <div class="data" >
            <div>Depth ft</div>
            {globalData.depth.toFixed(2)}
          </div>
        {/if}
        {#if globalConfig.seatempSensorName != ""}
          <div class="data" >
            <div>Water Temp (f)</div>
            {globalData.temp.toFixed(2)} f
          </div>
        {/if}
        <div class="data" >
          <div>Location</div>
          {@html globalData.pos.format(gpsFormatter)}
        </div>
        {#if globalConfig.movementSensorProps.compassHeadingSupported}
          <div class="data" >
            <div>Heading</div>
            {@html globalData.heading}
          </div>
        {/if}
        <table class="gauge" border="1">
          {#each gaugesToArray(globalData.gauges) as [key, value]}
            <tr>
              <th>{key}</th>
              <td>{value.Level.toFixed(0)} %</td>
              <td>{(value.Capacity * value.Level * 0.264172 / 100).toFixed(0)}</td>
              <td>/ {(value.Capacity * 0.264172).toFixed(0)}</td>
              {#if globalData.gaugesToHistorical[key]}
                <td>
                  <LinkedChart
                    data={gauageHistoricalToLinkedChart(globalData.gaugesToHistorical[key])}
                    width="100"
                    type="line"
                    scaleMax=100
                    linked="{key}"
                    uid="{key}"
                    barMinWidth="1"
                  />
                  <div style="position: absolute;">
                    <LinkedValue uid="{key}" />
                    <LinkedLabel linked="{key}"/>
                  </div>
                </td>
              {/if}
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
