<script lang="ts">
 import { getCookie } from 'typescript-cookie'
 import '@viamrobotics/prime-core/prime.css';
 import { onMount } from 'svelte';
 import { Icon as PrimeIcon } from '@viamrobotics/prime-core';

 
 import { Logger } from "tslog";
 
 import {Coordinate} from "tsgeo/Coordinate";
 import {DecimalMinutes}        from "tsgeo/Formatter/Coordinate/DecimalMinutes";

 import 'ol/ol.css';
 import ScaleLine from 'ol/control/ScaleLine.js';
 import {defaults as defaultControls} from 'ol/control/defaults.js';
 import Collection from 'ol/Collection.js';
 import {useGeographic} from 'ol/proj.js';
 import Map from 'ol/Map';
 import View from 'ol/View';
 import Point from 'ol/geom/Point.js';

 import {
   LinkedChart,
   LinkedLabel,
   LinkedValue
 } from "svelte-tiny-linked-charts"

 import * as VIAM from '@viamrobotics/sdk';

 import {
   setupLayers,
   updateOnLayers,
   findLayerByName,
   findOnLayerIndexOfName
 } from './lib/chart/setup.js';

 import {
   processRoute129285,
   updateAISFeatures,
   createTrackPoint,
   createTrackLine
 } from './lib/chart/features.js';

 import {
   msToKnots,
   pointDiff,
   celsiusToFahrenheit,
   fuelTotalLevel,
   fuelTotalCapacity,
   acPowerVoltAverage,
   acPowerAmpAt
 } from './lib/utils/conversions.js';

 import {
   dicToArray,
   moreThanOneFuelTank,
   gauageHistoricalToLinkedChart
 } from './lib/utils/helpers.js';

 import {
   getDataViaMQL,
   positionHistoryMQL,
   positionHistoryMQLNamed
 } from './lib/data/queries.js';

 import {
   filterResources,
   filterResourcesFirstMatchingName,
   filterResourcesAllMatchingNames,
   setupMovementSensor,
   discoverSensorNames
 } from './lib/data/sensors.js';

 let boatImage = "boat3.jpg";

 import { tankSort } from "./helpers.ts"
 
 const globalLogger = new Logger({ name: "global" });
 let globalClient: VIAM.RobotClient;
 let globalClientLastReset = new Date();
 let globalClientCloudMetaData = null;

 let globalCloudClient: VIAM.ViamClient;
 
 let globalData = $state({
   pos : new Coordinate(0,0),
   posHistory : [],
   posHistorical : [],
   posString : "n/a",
   speed : 0.0,
   temp : 0.0,
   depth : 0.0,
   heading: 0.0,
   windSpeed: 0.0,
   windAngle: 0.0,
   spotZeroFW : 0.0,
   spotZeroSW : 0.0,
   seakeeperData : {
     power_available: 0,
     power_enabled: 0,
     stabilize_available: false,
     stabilize_enabled: false,
     "progress_bar_percentage" : 0.0
   },
   gauges : {},
   acPowers : {},
   acPowerData : false,
   gaugesToHistorical : {},
   
   allResources : [],

   cameraNames : [],
   lastCameraTimes : [],
   
   numUpdates: 0,
   status: "Not connected yet",
   statusLastError: new Date(),
   lastData: new Date(),

   partConfig : {},
 });

 var globalConfig = $state({
   movementSensorName : "",
   movementSensorProps : {},
   movementSensorAlternates : [],
   movementSensorForQuery : "",
   
   aisSensorName : "",
   allPgnSensorName : "",
   seatempSensorName : "",
   depthSensorName : "",
   windSensorName : "",
   spotZeroFWSensorName : "",
   spotZeroSWSensorName : "",
   seakeeperSensorName : "",
   acPowers : [],
   
   zoomModifier : 0,
 });
 
 let mapGlobal = $state({

   map: null,
   view: null,

   aisFeatures: new Collection(),
   trackFeatures: new Collection(),
   routeFeatures: new Collection(),
   trackFeaturesLastCheck : new Date(0),
   myBoatMarker: null,
   
   inPanMode: false,
   lastZoom: 0,
   lastCenter: null,

   layerOptions: [],
   onLayers: new Collection(),
 });
 
 function gotNewData() {
   globalData.lastData = new Date();
 }

 function errorHandlerMaker(m) {
   return function(e) {
     return errorHandler(e, m);
   };
 }
 
 function errorHandler(e, context) {
   globalData.statusLastError = new Date();
   if (context) {
     globalLogger.error(context + " : " + e);
   } else {
     globalLogger.error(e);
   }
   var s = e.toString();
   globalData.status = "Error: " + s;
   if (context) {
     globalData.status = context + " : " + globalData.status;
   }
   
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
     globalData.status = "Forcing reconnect b/c of error: " + e.toString();
     globalClient = null;
     globalClientLastReset = new Date();
   }

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

     globalData.posHistory.push({"pos" : p});
     mapGlobal.trackFeatures.push(createTrackPoint([p.coordinate.longitude, p.coordinate.latitude]));

     var myPos = new Coordinate(p.coordinate.latitude, p.coordinate.longitude);
     globalData.pos = myPos;

     if (false) {
       // this is being stupid on mobile
       var gpsFormatter = new DecimalMinutes();
       gpsFormatter.setSeparator("\n")
                   .useCardinalLetters(true);
       
       globalData.posString = gpsFormatter.format(myPos);
     } else {
       globalData.posString = p.coordinate.latitude.toFixed(5) + ", " + p.coordinate.longitude.toFixed(5);
     }
     
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

     
     var sz = mapGlobal.map.getSize();
     var pp = [globalData.pos.lng, globalData.pos.lat];
     
     mapGlobal.myBoatMarker.setGeometry(new Point(pp));

     if (!mapGlobal.inPanMode) {
       mapGlobal.view.centerOn(pp, mapGlobal.map.getSize(), [sz[0]/2,sz[1]/2]);

       // zoom of 10 is about 30 miles
       // zoom of 16 is city level
       var zoom = Math.pow(Math.floor(globalData.speed),.45)
       zoom = Math.floor(16-zoom) + globalConfig.zoomModifier;
       if ( zoom <= 0 ) {
         zoom = 1;
       }
       mapGlobal.view.setZoom(zoom);

       mapGlobal.lastZoom = zoom;
       mapGlobal.lastCenter = pp;
     }
     
   }).catch(errorHandlerMaker("movement sensor"));
   
   msClient.getLinearVelocity().then((v) => {
     globalData.speed = msToKnots(v.y);
   }).catch(errorHandlerMaker("linear velocity"));
   
   msClient.getCompassHeading().then((ch) => {
     globalData.heading = ch;
   }).catch(errorHandlerMaker("compass"));


   if (globalConfig.seatempSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.seatempSensorName).getReadings().then((t) => {
       if (!isNaN(t.Temperature)) {
         globalData.temp = celsiusToFahrenheit(t.Temperature);
       }
     }).catch( errorHandlerMaker("seatemp") );
   }

   if (globalConfig.depthSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.depthSensorName).getReadings().then((d) => {
       globalData.depth = d.Depth * 3.28084;
     }).catch( (e) => {
       globalConfig.depthSensorName = "";
       errorHandler(e, "depth");
     });
   }

   if (globalConfig.windSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.windSensorName).getReadings().then((d) => {
       if (d["Reference"] == "True (ground referenced to North)") {
         globalData.windAngle = d["Wind Angle"];
         globalData.windSpeed = msToKnots(d["Wind Speed"]);
       }
     }).catch( (e) => {
       globalConfig.windSensorName = "";
       errorHandler(e, "wind");
     });
   }

   if (globalConfig.spotZeroFWSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.spotZeroFWSensorName).getReadings().then((d) => {
       globalData.spotZeroFW = d["Product Water Flow"] * 0.00440287;
     }).catch( (e) => {
       globalConfig.spotZeroFWSensorName = "";
       errorHandler(e, "spot zero fw");
     });
   }

   if (globalConfig.spotZeroSWSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.spotZeroSWSensorName).getReadings().then((d) => {
       globalData.spotZeroSW = d["Product Water Flow"] * 0.00440287;
     }).catch( (e) => {
       globalConfig.spotZeroSWSensorName = "";
       errorHandler(e, "spot zero sw");
     });
   }

   if (globalConfig.seakeeperSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.seakeeperSensorName).getReadings().then((d) => {
       globalData.seakeeperData = d;
     }).catch( (e) => {
       globalConfig.seakeeperSensorName = "";
       errorHandler(e, "seakeeper");
     });
   }

   globalConfig.acPowers.forEach( (acPowerName) => {
     new VIAM.SensorClient(client, acPowerName).getReadings().then((d) => {
       var n = acPowerName.split("ac-")[1];
       globalData.acPowers[n] = d;
       globalData.acPowerData = true;
     }).catch( errorHandlerMaker(acPowerName));
     
   });

   
   if (loopNumber % 30 == 2 ) {

     globalData.allResources.forEach( (r) => {
       if (r.subtype != "sensor") {
         return;
       }
       if (r.name.indexOf("fuel") < 0 && r.name.indexOf("freshwater") < 0) {
         return;
       }
       
       var sp = r.name.split(":");
       var name = sp[sp.length-1];

       new VIAM.SensorClient(client, r.name).getReadings().then((raw) => {
         globalData.gauges[name] = raw;
       }).catch( errorHandlerMaker(r.name) );
       
     });

     if (globalConfig.allPgnSensorName != "") {
       new VIAM.SensorClient(client, globalConfig.allPgnSensorName).getReadings().then((raw) => {

         for (var k in raw) {
           var doc = raw[k];
           if (k.indexOf("129285")==0){
             processRoute129285(doc, mapGlobal.routeFeatures);
           }
         }
       });
     }
     
     if (globalConfig.aisSensorName != "") {
       new VIAM.SensorClient(client, globalConfig.aisSensorName).getReadings().then((raw) => {
         updateAISFeatures(mapGlobal.aisFeatures, raw);
       }).catch( errorHandlerMaker("ais") );
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
     var cc = findComponentConfig(r.name);
     var skip = cc && cc.attributes && cc.attributes["chartplotter-hide"];

     if (skip) {
       if (removeCamera(r.name)) {
         console.log("removed camera: " + r.name);
       }
       return;
     }
     
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
     }).catch((e) => {
       removeCamera(r.name);
       errorHandler(e, r.name);
     });
     
   });

 }

 function removeCamera(n) {
   var idx = globalData.cameraNames.indexOf(n);
   if (idx >= 0) {
     globalData.cameraNames.splice(idx,1);
     return true;
   }
   return false;
 }

 async function updateResources(client: VIAM.RobotClient) {
   var resources = await client.resourceNames();
   globalData.allResources = resources;

   const movementSensorResult = await setupMovementSensor(client, resources);
   globalConfig.movementSensorName = movementSensorResult.movementSensorName;
   globalConfig.movementSensorProps = movementSensorResult.movementSensorProps;
   globalConfig.movementSensorAlternates = movementSensorResult.movementSensorAlternates;

   const sensorNames = discoverSensorNames(resources);
   globalConfig.aisSensorName = sensorNames.aisSensorName;
   globalConfig.allPgnSensorName = sensorNames.allPgnSensorName;
   globalConfig.seatempSensorName = sensorNames.seatempSensorName;
   globalConfig.depthSensorName = sensorNames.depthSensorName;
   globalConfig.windSensorName = sensorNames.windSensorName;
   globalConfig.spotZeroFWSensorName = sensorNames.spotZeroFWSensorName;
   globalConfig.spotZeroSWSensorName = sensorNames.spotZeroSWSensorName;
   globalConfig.seakeeperSensorName = sensorNames.seakeeperSensorName;
   globalConfig.acPowers = sensorNames.acPowers;

   console.log("globalConfig", $state.snapshot(globalConfig));

 }

 async function updateAndLoop() {
   globalData.numUpdates++;

   var timeSinceLastError = new Date() - globalData.statusLastError;
   if (timeSinceLastError > (120 * 1000) ) {
     globalData.status = "";
   }
   
   if (!globalClient) {
     try {
       globalClient = await connect();
       await updateResources(globalClient);

     } catch(error) {
       globalData.status = "Connect failed: " + error;
       globalClient = null;
     }
   } else if (globalData.numUpdates % 120 == 0) {
     await updateResources(globalClient);     
   }

   updateOnLayers(mapGlobal.layerOptions, mapGlobal.onLayers);
   
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

 function getHostAndCredentials() {
   const urlParams = new URLSearchParams(window.location.search);

   var host = urlParams.get("host");
   var apiKey = urlParams.get("api-key");
   var authEntity = urlParams.get("authEntity");

   if (!host || host == "") {
     console.log("yo");
     host = getCookie("host");
     apiKey = getCookie("api-key");
     authEntity = getCookie("api-key-id");
   }

   if (!host || host == "") {
     var machineId = window.location.pathname.split("/")[2];
     if (machineId != "") {
       var x = getCookie(machineId);
       if (x != "") {
         var x = JSON.parse(x);
         host = x.hostname;
         authEntity = x.id;
         apiKey = x.key;
       }
     }
   }
   
   const credential = {
     type: 'api-key',
     payload: apiKey,
     authEntity: authEntity
   };

   return [host, credential];
 }
 
 async function updateCloudDataAndLoop() {
   const [host, credential] = getHostAndCredentials();

   if (!globalCloudClient) {
     try {
       const opts: VIAM.ViamClientOptions = {
         credentials: credential,
       };
       
       globalCloudClient = await VIAM.createViamClient(opts);
       
     } catch( error ) {
       console.log("cannot connect to cloud: " + error);
     }
   }
   
   if (globalCloudClient) {
     try {
       await updateMachineConfig(globalCloudClient.appClient);
       await updateGaugeGraphs(globalCloudClient.dataClient);
     } catch ( error ) {
       console.log("updateGaugeGraphs error: " + error);
     }
   }

   setTimeout(updateCloudDataAndLoop, 1000);
 }

 async function updateMachineConfig(ac) {
   const part = await ac.getRobotPart(
     globalClientCloudMetaData.robotPartId
   )

   if (!part || !part.part) {
     throw new Error('Failed to get robot part: part or part.part is undefined')
   }
   
   globalData.partConfig = JSON.parse(part.configJson);
 }

 function findComponentConfig(n) {
   if (!globalData.partConfig) {
     return null;
   }

   if (!globalData.partConfig.components) {
     return null;
   }

   for ( var i=0; i < globalData.partConfig.components.length; i++) {
     var c = globalData.partConfig.components[i];
     if (c.name == n) {
       return c;
     }
   }
   return null;
 }

 function isComponentMethodHot(n, method) {
   var c = findComponentConfig(n);
   if (!c) {
     return false
   }

   var scs = c["service_configs"];
   if (!scs) {
     return false;
   }
   scs = scs.filter( (x) => x["type"] == "data_manager");
   for (var i=0; i < scs.length; i++) {
     var sc = scs[i];
     var cm = sc["attributes"]["capture_methods"];
     if (!cm) {
       continue;
     }
     var p = cm.filter( (x) => x["method"] == method );
     if (p.length < 1) {
       continue;
     }
     var pp = p[0];
     if (pp["recent_data_store"] && pp["recent_data_store"]["stored_hours"] >= 24) {
       return true;
     }
   }

   return false;
 }


 async function updateGaugeGraphs(dc, robotName) {
   var startTime = new Date(new Date() - 86400 * 1000);
   
   if (globalConfig.movementSensorName && globalData.posHistorical.length == 0) {
     // HACK HACK
     const urlParams = new URLSearchParams(window.location.search);
     var data = [];
     if (urlParams.get("host") == "boat-main.0pdb3dyxqg.viam.cloud" && urlParams.get("authEntity")[0] == "a") {
       var foo = await fetch("https://us-central1-eliothorowitz.cloudfunctions.net/albertboat?d=" + startTime, { method : 'GET' });
       var bar = await foo.json();
       data = bar.data;
     } else {
       data = await positionHistoryMQL(dc, startTime, globalConfig, globalClientCloudMetaData);
     }
     
     mapGlobal.trackFeatures.clear();

     var first = [];
     var prev = [];
     for ( var i = 0; i < data.length; i++) {
       var p = data[i];
       var x = [p.pos.coordinate.longitude, p.pos.coordinate.latitude];
       mapGlobal.trackFeatures.push(createTrackPoint(x));
       if ( i > 0 ) {
         mapGlobal.trackFeatures.push(createTrackLine(x, prev));
       }
       prev = x;
       if (first.length == 0) {
         first = x;
       }
     }

     if (first.length > 0 && globalData.posHistory.length > 0) {
       var p = globalData.posHistory[0];
       var x = [p.pos.coordinate.longitude, p.pos.coordinate.latitude];
       mapGlobal.trackFeatures.push(createTrackLine(x, first));
     }
     
     mapGlobal.trackFeaturesLastCheck = new Date();
     globalData.posHistorical = data;
   }

   for ( var g in globalData.gauges ) {
     var h = globalData.gaugesToHistorical[g];
     if (h && (new Date() - h.ts) < 60000) {
       continue;
     }


     var timeStart = new Date();
     var data = await getDataViaMQL(dc, g, startTime, globalClientCloudMetaData);
     var getDataTime = (new Date()).getTime() - timeStart.getTime();
     
     console.log("time to get graph data for " + g + " took " + getDataTime + " and had " + data.length + " points");
     
     h = { ts : new Date(), data : data };
     globalData.gaugesToHistorical[g] = h;
   }

 }
 
 async function connect(): VIAM.RobotClient {
   const [host, credential] = getHostAndCredentials();
   
   var c = await VIAM.createRobotClient({
     host,
     credentials: credential,
     signalingAddress: 'https://app.viam.com:443',

     // optional: configure reconnection options
     reconnectMaxAttempts: 20,
     reconnectMaxWait: 5000,
   });
   
   globalData.status = "Connected";
   
   globalLogger.info('connected!');
   
   c.on('disconnected', disconnected);
   c.on('reconnected', reconnected);

   globalClientCloudMetaData = await c.getCloudMetadata();
   console.log(globalClientCloudMetaData);
   return c;
 }

 async function disconnected(event) {
   globalData.status = "Disconnected";
   globalLogger.warn('The robot has been disconnected. Trying reconnect...');
 }

 async function reconnected(event) {
   globalData.status = "Connected";
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
   const urlParams = new URLSearchParams(window.location.search);
   var temp = urlParams.get("zoomModifier");
   if (temp) {
     temp = parseInt(temp);
     globalConfig.zoomModifier = temp;
   }
   useGeographic();
   
   mapGlobal.layerOptions = setupLayers(
     mapGlobal.aisFeatures,
     mapGlobal.trackFeatures,
     mapGlobal.routeFeatures,
     boatImage,
     () => globalData.heading
   );

   // Extract myBoatMarker from the layerOptions
   const boatLayer = findLayerByName(mapGlobal.layerOptions, "boat");
   if (boatLayer) {
     const features = boatLayer.layer.getSource().getFeatures();
     if (features.length > 0) {
       mapGlobal.myBoatMarker = features[0];
     }
   }

   mapGlobal.view = new View({
     center: [0, 0],
     zoom: 15
   });

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

   updateOnLayers(mapGlobal.layerOptions, mapGlobal.onLayers);
   updateOnLayers(mapGlobal.layerOptions, mapGlobal.onLayers);
 }

 onMount(start);

 function seakeeper(name, value) {
   var cmd = {};
   cmd[name] = value;
   console.log("sending to: "+ globalConfig.seakeeperSensorName);
   console.log(cmd);

   new VIAM.SensorClient(globalClient, globalConfig.seakeeperSensorName).doCommand(VIAM.Struct.fromJson(cmd)).then((r) => {
     console.log(r);
   }).catch( (e) => {
     errorHandler(e);
   });
   
   return true;
 }
</script>


<main class="w-dvw lg:h-dvh p-2 grid grid-cols-1 lg:grid-cols-4 grid-rows-3 lg:grid-rows-6 gap-2 bg-black">
  <div id="map-container" class="relative lg:col-span-3 row-span-3 lg:row-span-5 border border-dark">
    <div id="map" class="min-h-[50dvh] h-fit bg-white"></div>
    <div class="absolute bottom-0 right-0">
      {#if mapGlobal.inPanMode}
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
  
  <aside class="lg:row-span-6 flex flex-col gap-4 border border-dark p-1 min-h-full text-white">
    {#if globalData.status === "Connected"}
      <div class="flex gap-2 items-center w-full min-h-12 px-2 border border-success-medium">
        <PrimeIcon
          name="broadcast"
          cx="text-success-dark"
        />
        <div class="text-sm text-success-dark">{globalData.status}</div>
      </div>
    {:else}
    <div class="flex gap-2 items-center w-full min-h-12 px-2 border border-info-medium">
      <PrimeIcon
        name="broadcast-off"
        cx="text-info-dark"
      />
      <div class="text-sm text-info-dark">{globalData.status}</div>
    </div>
    {/if}

    <div id="navData" class="flex flex-col divide-y">
      {#if globalConfig.movementSensorProps.linearVelocitySupported}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">SOG<br></div>
          <div>
            <span class="font-bold">{globalData.speed.toFixed(2)}</span>
            <sup>knots</sup>  
          </div>
        </div>
      {/if}
      {#if globalConfig.depthSensorName != ""}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Depth</div>
          <div>
            <span class="font-bold">{globalData.depth.toFixed(2)}</span>
            <sup>ft</sup> 
          </div>
        </div>
      {/if}
      {#if globalConfig.windSensorName != ""}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Wind Direction</div>
          <div>
            <span class="font-bold">{globalData.windAngle.toFixed(0)}</span>
            <sup>degrees</sup>
          </div>
        </div>
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Wind Speed</div>
          <div>
            <span class="font-bold">{globalData.windSpeed.toFixed(1)}</span>
            <sup>kn</sup>
          </div>
        </div>

      {/if}

      {#if globalConfig.seatempSensorName != ""}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Water Temp</div>
          <div>
            <span class="font-bold">{globalData.temp.toFixed(2)}</span>
            <sup>f</sup>
          </div>
        </div>
      {/if}
      <div class="flex gap-2 p-2 text-lg">
        <div class="min-w-32">Location</div>
        <span><small>{globalData.posString}</small></span>
      </div>
      {#if globalConfig.movementSensorProps.compassHeadingSupported}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Heading</div>
          <div>
            <span class="font-bold">{@html globalData.heading.toFixed(2)}</span>
          </div>
        </div>
      {/if}
      {#if globalConfig.spotZeroFWSensorName != ""}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">SpotZero F/S</div>
          <div>
            <span class="font-bold">{@html globalData.spotZeroFW.toFixed(2)}</span> /
            <span class="font-bold">{@html globalData.spotZeroSW.toFixed(2)}</span>
            gpm
          </div>
        </div>
      {/if}
      {#if globalConfig.seakeeperSensorName != ""}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Seakeeper </div>
          <div>
            <span class="font-bold">
              {#if globalData.seakeeperData["power_enabled"] >= 1}
                <button on:click={() => seakeeper('power',false)}>P</button>
              {:else if globalData.seakeeperData["power_available"] >= 1 }
                <button on:click={() => seakeeper('power', true)}>p</button>
              {/if}
              {#if globalData.seakeeperData["stabilize_enabled"] >= 1}
                <button on:click={() => seakeeper('enable',false)}>E</button>
              {:else if globalData.seakeeperData["stabilize_available"] >= 1 }
                <button on:click={() => seakeeper('enable',true)}>e</button>
              {/if}
              {@html globalData.seakeeperData["progress_bar_percentage"].toFixed(2)}%
              ({globalData.seakeeperData["flywheel_speed"]})
            </span>
          </div>
        </div>
      {/if}
      <div class="flex flex-col divide-y">
        {#each dicToArray(globalData.gauges, tankSort) as [key, value]}
          <section class="overflow-visible flex gap-2 p-2 text-lg">
            <h2 class="min-w-32 capitalize">{key}</h2>
            <div class="grow">
              <div class="flex gap-1 font-bold">
                <div>{value.Level.toFixed(0)} %</div>
                <div>{(value.Capacity * value.Level * 0.264172 / 100).toFixed(0)}</div>
                <div>/ {(value.Capacity * 0.264172).toFixed(0)}</div>
              </div>
              {#if globalData.gaugesToHistorical[key]}
              <div class="relative">
                <div role="article" tabindex="-1" class="peer bg-dark hover:cursor-pointer">
                  <LinkedChart
                    data={gauageHistoricalToLinkedChart(globalData.gaugesToHistorical[key])}
                    style="width: 100%;"
                    width="100"
                    type="line"
                    lineColor="#0000ff"
                    scaleMax=100
                    linked="{key}"
                    uid="{key}"
                    barMinWidth="1"
                    grow
                  />
                </div>
                <div
                  class="hidden peer-hover:block z-10 text-nowrap -bottom-8 right-1 absolute border border-medium px-2 w-fit"
                >
                  <LinkedValue uid="{key}" />
                  <LinkedLabel linked="{key}"/>
                </div>
              </div>
            {/if}   
            </div>
          </section>
        {/each}
        {#if moreThanOneFuelTank(globalData.gauges)}
          <section>
            <div class="flex gap-2 p-2 text-lg">
              <div class="min-w-32">Total Fuel </div>
              <div>
                <span class="font-bold">{fuelTotalLevel(globalData.gauges)} / {fuelTotalCapacity(globalData.gauges)} gallons</span>
              </div>
            </div>
          </section>
        {/if}
      </div>
      {#if globalData.acPowerData}
        <div class="flex gap-2 p-2">
          <div class="min-w-32">AC Power</div>
          <div style="font-size:.7em;">
            <table class="text-white">
              <tbody>
                <tr>
                  <td></td>
                  <th>Voltage</th>
                  <th>Current</th>
                </tr>
                {#each dicToArray(globalData.acPowers) as [name, d]}
                  <tr>
                    <th>{name}</th>
                    <td>{d["Line-Neutral AC RMS Voltage"]}</td>
                    <td>{d["AC RMS Current"]}</td>
                  </tr>
                {/each}
                <tr>
                  <th>Ttl</th>
                  <td>{acPowerVoltAverage(globalData.acPowers).toFixed(0)}</td>
                  <td>{acPowerAmpAt(acPowerVoltAverage(globalData.acPowers), globalData.acPowers).toFixed(0)}</td>
                </tr>
                <tr>
                  <th>Ttl</th>
                  <td>{(2*acPowerVoltAverage(globalData.acPowers)).toFixed(0)}</td>
                  <td>{acPowerAmpAt(2*acPowerVoltAverage(globalData.acPowers), globalData.acPowers).toFixed(0)}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      {/if}
    </div>

    <div class="grow text-xs flex flex-col flex-col-reverse text-gray-500 text-right">{globalData.numUpdates}</div>
  </aside>

  <div class="h-[50dvh] lg:h-[auto] overflow-x-auto flex lg:col-span-3 border border-dark p-1 ">
    {#each globalData.cameraNames as name, index}
      <img id="{name}" class="w-full lg:w-[250px]" alt="{name}" />
    {/each}
  </div>

  <div>
    <h3>Powered By</h3>
    <img src="https://app.viam.com/static/images/viam-logo.png" width="250" height="49" alt="viam logo" style="filter: invert(1);" />
  </div>

</main>
