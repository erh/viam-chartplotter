<script lang="ts">
 import { getCookie } from 'typescript-cookie'
 // import '@viamrobotics/prime-core/prime.css';
 import { onMount, onDestroy } from 'svelte';
 import { Icon as PrimeIcon } from '@viamrobotics/prime-core';

 
 import { Logger } from "tslog";
import type { BoatInfo } from './lib/BoatInfo';
 
 import {Coordinate} from "tsgeo/Coordinate";
 import {DecimalMinutes}        from "tsgeo/Formatter/Coordinate/DecimalMinutes";

 import { BSON } from "bsonfy";

 import {
   LinkedChart,
   LinkedLabel,
   LinkedValue
 } from "svelte-tiny-linked-charts"

 import * as VIAM from '@viamrobotics/sdk';

 import { tankSort } from "./helpers.ts"
 import MarineMap from "./marineMap.svelte"
 
 const globalLogger = new Logger({ name: "global" });
 let globalClient: VIAM.RobotClient;
 let globalClientLastReset = new Date();
 let globalClientCloudMetaData = null;

 let globalCloudClient: VIAM.ViamClient;
 
 // Track timeout IDs and blob URLs for cleanup
 let updateLoopTimeout: number | undefined;
 let cloudLoopTimeout: number | undefined;
 let cameraBlobUrls: Record<string, string> = {};
 
 let globalData = $state({
   pos : new Coordinate(0,0),
   posHistory : [],
   posHistoryLastCheck : 0,
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
   machineStatus : {
     resources: []
   },

   cameraNames : [],
   lastCameraTimes : [],
   
   numUpdates: 0,
   status: "Not connected yet",
   statusLastError: new Date(),
   lastData: new Date(),

   partConfig : {},
   aisBoats : [] as BoatInfo[],
 });

 var globalConfig = $state({
   movementSensorName : "",
   movementSensorProps : {},
   movementSensorAlternates : [],
   movementSensorForQuery : "",
   
   aisSensorName : "",
   routeSensorName : "",
   seatempSensorName : "",
   depthSensorName : "",
   windSensorName : "",
   spotZeroFWSensorName : "",
   spotZeroSWSensorName : "",
   seakeeperSensorName : "",
   acPowers : [],
   
   zoomModifier : 0,
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

 function doUpdate(loopNumber: int, client: VIAM.RobotClient){
   const msClient = new VIAM.MovementSensorClient(client, globalConfig.movementSensorName);
   
   msClient.getPosition().then((p) => {
     gotNewData();

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

     
   }).catch(errorHandlerMaker("movement sensor"));
   
   msClient.getLinearVelocity().then((v) => {
     globalData.speed = v.y * 1.94384;
   }).catch(errorHandlerMaker("linear velocity"));
   
   msClient.getCompassHeading().then((ch) => {
     globalData.heading = ch;
   }).catch(errorHandlerMaker("compass"));


   if (globalConfig.seatempSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.seatempSensorName).getReadings().then((t) => {
       if (!isNaN(t.Temperature)) {
         globalData.temp = 32 + (t.Temperature * 1.8);
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
         globalData.windSpeed = d["Wind Speed"] * 1.94384;
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

   if (globalConfig.routeSensorName != "") {
     new VIAM.SensorClient(client, globalConfig.routeSensorName).getReadings().then((raw) => {
       globalData.route = raw;
     }).catch( function(e) {
       globalData.route = {};
     });
   }
   
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

     if (globalConfig.aisSensorName != "") {
       new VIAM.SensorClient(client, globalConfig.aisSensorName).getReadings().then((raw) => {
         var good = [];
         
         for ( var mmsi in raw  ) {
           
           var rawBoat = raw[mmsi];
           if (rawBoat == null || rawBoat.Location == null || rawBoat.Location.length != 2 || rawBoat.Location[0] == null) {
             continue;
           }
           
          var boat = {
            name: rawBoat.Name || "",
            location: rawBoat.Location,
            speed: rawBoat.Speed || 0,
            heading: rawBoat.Heading || 0,
            mmsi: mmsi
          };
           
           good.push(boat);
         }
         
         globalData.aisBoats = good;

       }).catch( (e) => {
         globalData.aisBoats = [];
         errorHandler(e, "ais");
       });
     }
   }
 }

 function acPowerVoltAverage(data) {
   var total = 0;
   var num = 0;
   
   for (var k in data) {
     var dd = data[k];
     total += dd["Line-Neutral AC RMS Voltage"];
     num++;
   }
   
   return total / num;
 }
 
 function acPowerAmpAt(vvv, data) {
   var total = 0;
   
   for (var k in data) {
     var dd = data[k];
     var a = dd["AC RMS Current"];
     var v = dd["Line-Neutral AC RMS Voltage"];
     var w = a * v;
     total += w / vvv;
   }
   
   return total;
   
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
           // Revoke old blob URL before creating new one to prevent memory leak
           if (cameraBlobUrls[r.name]) {
             URL.revokeObjectURL(cameraBlobUrls[r.name]);
           }
           const newUrl = URL.createObjectURL(new Blob([img]));
           cameraBlobUrls[r.name] = newUrl;
           i.src = newUrl;
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
 
 function filterResourcesFirstMatchingName(resources, t, st, n) {
   var matching = filterResources(resources, t, st, n);
   if (matching.length > 0) {
     return matching[0].name;
   }
   return "";
 }

 function filterResourcesAllMatchingNames(resources, t, st, n) {
   var matching = filterResources(resources, t, st, n);
   var names = [];
   for ( var r of matching) {
     names.push(r.name);
   }
   return names.sort();
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

   const machineStatus = await client.getMachineStatus();
   globalData.machineStatus = machineStatus;
   console.log(globalData.machineStatus);

   await setupMovementSensor(client, resources);

   globalConfig.aisSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /\bais$/);
   globalConfig.routeSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /\broute$/);
   globalConfig.seatempSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /\bseatemp$/);
   globalConfig.depthSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /depth/);
   globalConfig.windSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /wind/);
   globalConfig.spotZeroFWSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /spotzero-fw/);
   globalConfig.spotZeroSWSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /spotzero-sw/);
   globalConfig.seakeeperSensorName = filterResourcesFirstMatchingName(resources, "component", "sensor", /seakeeper/);
   globalConfig.acPowers = filterResourcesAllMatchingNames(resources, "component", "sensor", /\bac-\d-\d$/);

   console.log("globalConfig", $state.snapshot(globalConfig));

 }
 
 async function setupMovementSensor(client: VIAM.RobotClient, resources) {
   resources = filterResources(resources, "component", "movement_sensor", null);

   var allGpsNames = [];
   
   // pick best movement sensor
   var bestName = "";
   var bestScore = 0;
   var bestProp = {};
   
   for (var r of resources) {
     const msClient = new VIAM.MovementSensorClient(client, r.name);
     var prop = await msClient.getProperties();

     var score = 0;
     if (prop.positionSupported) {
       console.log(r)
       allGpsNames.push(r.name);
       score++;
     }
     if (prop.linearVelocitySupported) {
       score++;
     }
     if (prop.compassHeadingSupported) {
       score++;
     }

     //console.log(r.name + " : " + score);
     
     if (score > bestScore || (score == bestScore && r.name.length < bestName.length) ) {
       bestName = r.name;
       bestScore = score;
       bestProp = prop;
     }

   }
   
   globalConfig.movementSensorName = bestName;
   globalConfig.movementSensorProps = bestProp;
   globalConfig.movementSensorAlternates = allGpsNames;
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
   
   var client = globalClient;
   
   if (client) {
     doUpdate(globalData.numUpdates, client);
     doCameraLoop(globalData.numUpdates, client);
   }

   updateLoopTimeout = setTimeout(updateAndLoop, 1000);
   if (globalData.numUpdates == 1) {
     cloudLoopTimeout = setTimeout(updateCloudDataAndLoop, 1000);
   }
 }

 function getHostAndCredentials() {
   const urlParams = new URLSearchParams(window.location.search);

   var host = urlParams.get("host");
   var apiKey = urlParams.get("api-key");
   var authEntity = urlParams.get("authEntity");

   if (!host || host == "") {
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

       var opts: VIAM.ViamClientOptions = {
         serviceHost: "https://app.viam.com",
         credentials: credential,
       };
       
       var userTokenCookie = getCookie("userToken");
       if (userTokenCookie) {
         const startIndex = userTokenCookie.indexOf("{");
         const endIndex = userTokenCookie.indexOf("}");
         userTokenCookie = userTokenCookie.slice(startIndex, endIndex+1);

         const {access_token: accessToken} = JSON.parse(userTokenCookie);
         opts.credentials = {
           type: "access-token",
           payload: accessToken
         }
         console.log("new credentials", opts.credentials);
       }
       
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

   cloudLoopTimeout = setTimeout(updateCloudDataAndLoop, 1000);
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
 
 async function getDataViaMQL(dc, g, startTime) {
   var match = {
     "location_id" : globalClientCloudMetaData.locationId,
     "robot_id" : globalClientCloudMetaData.machineId,
     "component_name" : g,
     time_received: { $gte: startTime }
   };
   
   var group = {
     "_id": { "$concat" : [
                                      { "$toString": { "$substr" : [ { "$year": "$time_received" } , 2, -1 ] } },
                                      "-",
                                      { "$toString" : { "$month": "$time_received" } },
                                      "-",
                                      { "$toString" : { "$dayOfMonth": "$time_received" } },
                                      " ",
                                      { "$toString" : { "$hour": "$time_received" } },
                                      ":",
                                      { "$toString" : { "$multiply" : [ 15, { "$floor" : { "$divide": [ { "$minute": "$time_received"}, 15] } } ] } }
                                      ] },
     "ts" : { "$min" : "$time_received" },
     "min" : { "$min" : "$data.readings.Level" },
     "max" : { "$max" : "$data.readings.Level" }
   };
   
   var query = [
     BSON.serialize( { "$match" : match } ),
     BSON.serialize( { "$group" : group } ),
     BSON.serialize( { "$sort" : { ts : -1 } } ),
     BSON.serialize( { "$limit" : (24 * 4) } ),
     BSON.serialize( { "$sort" : { ts : 1 } } ),
   ];

   var data = await dc.tabularDataByMQL(globalClientCloudMetaData.primaryOrgId, query, true);

   return data;
 }

 async function positionHistoryMQL(dc, startTime) {
   if (globalConfig.movementSensorForQuery != "") {
     var res = await positionHistoryMQLNamed(dc, startTime, globalConfig.movementSensorForQuery);
     if (res.length > 0) {
       return res;
     }
   }
   
   for (var i=0; i<globalConfig.movementSensorAlternates.length; i++){
     var n = globalConfig.movementSensorAlternates[i];
     
     res = await positionHistoryMQLNamed(dc, startTime, n);
     if (res.length > 0) {
       globalConfig.movementSensorForQuery = n;
       return res;
     }
     
   }
   return res;
 }
 
 function findComponentStatus(n) {
   for (var i=0; i<globalData.machineStatus.resources.length; i++) {
     var x = globalData.machineStatus.resources[i];
     if (x.name.name == n) {
       return x.cloudMetadata;
     }
   }
   return null;
 }
 
 async function positionHistoryMQLNamed(dc, startTime, n) {
   var name = n.split(":");

   var orgId = globalClientCloudMetaData.primaryOrgId;
   
   var match = {
   "location_id" : globalClientCloudMetaData.locationId,
   "robot_id" : globalClientCloudMetaData.machineId,
   "component_name" : name[name.length-1],
   "method_name" : "Position",
   "time_received": { $gte: startTime }
   };

   var compStatus = findComponentStatus(n);
   if (compStatus) {
     match.location_id = compStatus.locationId;
     match.robot_id = compStatus.machineId;
     orgId = compStatus.primaryOrgId;
     console.log("newInfo", match, orgId)
   }
   
   var group = {
     "_id": { "$concat" : [
                                      { "$toString": { "$substr" : [ { "$year": "$time_received" } , 2, -1 ] } },
                                      "-",
                                      { "$toString" : { "$month": "$time_received" } },
                                      "-",
                                      { "$toString" : { "$dayOfMonth": "$time_received" } },
                                      " ",
                                      { "$toString" : { "$hour": "$time_received" } },
                                      ":",
                                      { "$toString" : { "$minute": "$time_received"} },
                                      ] },
     "ts" : { "$min" : "$time_received" },
     "pos" : { "$first" : "$data" },
   };
   
   
   var query = [
     BSON.serialize( { "$match" : match } ),
     BSON.serialize( { "$sort" : { time_received : -1 } } ),
     BSON.serialize( { "$group" : group } ),
     BSON.serialize( { "$sort" : { ts : -1 } } ),
   ];

   var hot = true;//isComponentMethodHot(n, "Position");
   
   var timeStart = new Date();
   var data = await dc.tabularDataByMQL(orgId, query, hot);
   var getDataTime = (new Date()).getTime() - timeStart.getTime();
   console.log("got " + data.length + " history data points from:" + n + " in " + getDataTime + "ms hot: " + hot);
   /*
   if (data.length > 0) {
     console.log("first : " + data[0]._id + " " + data[0].ts.getTime() + " " + data[0].pos.coordinate.latitude);
   }
   */
   return data;
 }

 async function updatePositionHistory(dc, robotName, startTime) {
   if (!globalConfig.movementSensorName) {
     return;
   }
   var timeSince = (new Date()) - globalData.posHistoryLastCheck;
   if (timeSince < (120  * 1000)) {
     return;
   }
   
   // HACK HACK
   const urlParams = new URLSearchParams(window.location.search);
   var data = [];
   if (urlParams.get("host") == "boat-main.0pdb3dyxqg.viam.cloud" && urlParams.get("authEntity")[0] == "a") {
     var foo = await fetch("https://us-central1-eliothorowitz.cloudfunctions.net/albertboat?d=" + startTime, { method : 'GET' });
     var bar = await foo.json();
     data = bar.data;
   } else {
     data = await positionHistoryMQL(dc, startTime);
   }

   data = data.map( (raw) => {
     return {lat: raw.pos.coordinate.latitude, lng: raw.pos.coordinate.longitude};
   });

   // Limit position history to 7 days (prevents unbounded memory growth)
   const MAX_HISTORY_POINTS = 7 * 24 * 60; // 7 days * 24 hours * 60 minutes = 10,080 points
   if (data.length > MAX_HISTORY_POINTS) {
     data = data.slice(-MAX_HISTORY_POINTS);
   }

   globalData.posHistoryLastCheck = new Date();
   globalData.posHistory = data;
 }
 
 async function updateGaugeGraphs(dc, robotName) {
   var startTime = new Date(new Date() - 86400 * 1000);

   updatePositionHistory(dc, robotName, startTime);


   for ( var g in globalData.gauges ) {
     var h = globalData.gaugesToHistorical[g];
     if (h && (new Date() - h.ts) < 60000) {
       continue;
     }


     var timeStart = new Date();
     var data = await getDataViaMQL(dc, g, startTime);
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

   // Update page title with hostname
   document.title = `Viam Chartplotter - ${host}`;

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
     updateAndLoop();
     return {}     
   } catch (error) {
     errorHandler(error);
     console.log(error.stack);
   }
 }

 onMount(start);
 
 onDestroy(() => {
   // Clear timeout loops to prevent memory leaks
   if (updateLoopTimeout !== undefined) {
     clearTimeout(updateLoopTimeout);
   }
   if (cloudLoopTimeout !== undefined) {
     clearTimeout(cloudLoopTimeout);
   }
   
   // Revoke all blob URLs to free memory
   Object.values(cameraBlobUrls).forEach(url => {
     URL.revokeObjectURL(url);
   });
   cameraBlobUrls = {};
   
   // Disconnect client event listeners if present
   if (globalClient) {
     try {
       // Remove any event listeners attached to the client
       globalClient.removeAllListeners?.();
     } catch (e) {
       console.log("Error cleaning up client:", e);
     }
   }
 });

 function moreThanOneFuelTank(gauges) {
   var found = false;
   for (var k in gauges) {
     var g = gauges[k];
     if (g["Type"] == "Fuel"){
       if (found) {
         return true;
       }
       found = true;
     }
   }
   return false;
 }

 function fuelTotalLevel(gauges) {
   var total = 0;
   for (var k in gauges) {
     var g = gauges[k];
     if (g["Type"] != "Fuel"){
       continue;
     }

     total += g["Level"] * g["Capacity"] / 100;

   }
   return Math.round(total * .264172);
 }

 function fuelTotalCapacity(gauges) {
   var total = 0;
   for (var k in gauges) {
     var g = gauges[k];
     if (g["Type"] != "Fuel"){
       continue;
     }

     total += g["Capacity"];

   }
   return Math.round(total * .264172);
 }

 function dicToArray(d, sortFunction) {
   var names = Object.keys(d);
   if (sortFunction) {
     names = sortFunction(names);
   } else {
     names.sort();
   }

   var a = [];
   
   for ( var i = 0; i < names.length; i++) {
     var n = names[i];
     a.push( [ n, d[n] ]);
   }
   return a;
 }

 function gauageHistoricalToLinkedChart(data) {
   var res = {};
   for (var d in data.data) {
     var dd = data.data[d];
     res[dd._id] = Math.floor(dd.min)
   }
   return res;
 }

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

  <MarineMap myBoat={{
    name: "me",
    location: [globalData.pos.getLat(), globalData.pos.getLng()],
    speed: globalData.speed,
    heading: globalData.heading,
    route: globalData.route ? {
      destinationLongitude: globalData.route["Destination Longitude"],
      destinationLatitude: globalData.route["Destination Latitude"],
      distanceToWaypoint: globalData.route["Distance to Waypoint"],
      waypointClosingVelocity: globalData.route["Waypoint Closing Velocity"]
    } : undefined
  }} zoomModifier={globalConfig.zoomModifier} boats={globalData.aisBoats} positionHistorical={globalData.posHistory}>
  </MarineMap>
    
  
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
      {#if globalData.route && globalData.route["Distance to Waypoint"] > 0}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Next Waypoint</div>
          <div>
            <div class="font-bold">{(globalData.route["Distance to Waypoint"] * 0.000539957).toFixed(2)} nm</div>
            <div class="font-bold">{((globalData.route["Distance to Waypoint"] / globalData.route["Waypoint Closing Velocity"]) / 60).toFixed(1)} minutes</div>
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
                <button onclick={() => seakeeper('power',false)}>P</button>
              {:else if globalData.seakeeperData["power_available"] >= 1 }
                <button onclick={() => seakeeper('power', true)}>p</button>
              {/if}
              {#if globalData.seakeeperData["stabilize_enabled"] >= 1}
                <button onclick={() => seakeeper('enable',false)}>E</button>
              {:else if globalData.seakeeperData["stabilize_available"] >= 1 }
                <button onclick={() => seakeeper('enable',true)}>e</button>
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
                    scaleMax={100}
                    linked={key}
                    uid={key}
                    barMinWidth="1"
                    grow
                  />
                </div>
                <div
                  class="hidden peer-hover:block z-10 text-nowrap -bottom-8 right-1 absolute border border-medium px-2 w-fit"
                >
                  <LinkedValue uid={key} />
                  <LinkedLabel linked={key}/>
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

    <div class="grow text-xs flex flex-col-reverse text-gray-500 text-right">{globalData.numUpdates}</div>
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
