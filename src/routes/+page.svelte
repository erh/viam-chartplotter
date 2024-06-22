<script lang="ts">
 import '../output.css'
 import '@viamrobotics/prime-core/prime.css';
 import { onMount } from 'svelte';
 import { Icon as PrimeIcon } from '@viamrobotics/prime-core';

 
 import { Logger } from "tslog";
 
 import {Coordinate} from "tsgeo/Coordinate";
 import {DMS}        from "tsgeo/Formatter/Coordinate/DMS";

 import { BSON } from "bsonfy";

 import Collection from 'ol/Collection.js';
 import {useGeographic} from 'ol/proj.js';
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

 import { base } from '$app/paths';
 let boatImage = base + "/boat3.jpg";
 
 let gpsFormatter = new DMS();
 gpsFormatter.setSeparator("<br>")
             .useCardinalLetters(true)
             .setUnits(DMS.UNITS_ASCII);

 
 const globalLogger = new Logger({ name: "global" });
 let globalClient: VIAM.RobotClient;
 let globalClientLastReset = new Date();
 let globalClientCloudMetaData = null;

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
   status: "Not connected yet",
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
   trackFeatures: new Collection(),
   trackFeaturesLastCheck : new Date(0),
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
   globalData.status = "Error: " + s;

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

     
     var sz = mapGlobal.map.getSize();
     var pp = [globalData.pos.lng, globalData.pos.lat];
     
     mapGlobal.myBoatMarker.setGeometry(new Point(pp));

     if (!mapGlobal.inPanMode) {
       mapGlobal.view.centerOn(pp, mapGlobal.map.getSize(), [sz[0]/2,sz[1]/2]);

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
       globalData.status = "Connect failed: " + error;
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

     await updateGaugeGraphs(globalCloudClient.dataClient, robotName);
   }

   setTimeout(updateCloudDataAndLoop, 1000);
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
   
   var data = await dc.tabularDataByMQL(globalClientCloudMetaData.primaryOrgId, query);

   return data;
 }

 async function getDataViaRaw(dc, robotName, g, startTime) {
   var f = dc.createFilter({
     robotName: robotName,
     organizationIdsList: [globalClientCloudMetaData.primaryOrgId],
     locationIdsList: [globalClientCloudMetaData.locationId],
     startTime: startTime,
     componentName: g,
   });
   
   var data = await dc.tabularDataByFilter(f);

   var m = {};
   
   data.forEach( (d) => {
     var ts = d.timeReceived;
     var key = (ts.getYear() - 100) + "-" + (1 + ts.getMonth()) + "-" + ts.getDate() + "-" + ts.getHours() + "-";
     key += Math.floor(ts.getMinutes() / 15) * 15;
     var r = d.data.readings;
     var x = { _id : key, ts : ts , min : r.Level, max : r.Level };
     m[key] = x; // TODO fix  me
     
   } );

   var all = [];
   for ( var k in m ) {
     all.push(m[k]);
   }

   all.sort( function(a,b){
     return a.ts.getTime() < b.ts.getTime();
   });
   
   return all;
 }

 async function positionHistoryMQL(dc, startTime) {
   var name = globalConfig.movementSensorName.split(":");
   
   var match = {
     "location_id" : globalClientCloudMetaData.locationId,
     "robot_id" : globalClientCloudMetaData.machineId,
     "component_name" : name[name.length-1],
     "method_name" : "Position",
     "time_received": { $gte: startTime }
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
     "pos" : { "$first" : "$data" },
   };
   
   
   var query = [
     BSON.serialize( { "$match" : match } ),
     BSON.serialize( { "$sort" : { ts : -1 } } ),
     BSON.serialize( { "$group" : group } ),
     BSON.serialize( { "$sort" : { ts : -1 } } ),
   ];
   
   var data = await dc.tabularDataByMQL(globalClientCloudMetaData.primaryOrgId, query);
   console.log("got " + data.length + " history data points");
   return data;
 }

 
 async function updateGaugeGraphs(dc, robotName) {
   var startTime = new Date(new Date() - 86400 * 1000);
   
   if (globalConfig.movementSensorName && ( new Date() - mapGlobal.trackFeaturesLastCheck ) > 60000 ) {

     // HACK HACK
     const urlParams = new URLSearchParams(window.location.search);
     var data = [];
     if (urlParams.get("host") == "boat-main.0pdb3dyxqg.viam.cloud" && urlParams.get("authEntity")[0] == "a") {
       console.log("eliot")
       var foo = await fetch("https://us-central1-eliothorowitz.cloudfunctions.net/albertboat?d=" + startTime, { method : 'GET' });
       var bar = await foo.json();
       data = bar.data;
     } else {
       data = await positionHistoryMQL(dc, startTime);
     }
     
     mapGlobal.trackFeatures.clear();

     var prev = [];
     for ( var i = 0; i < data.length; i++) {
       var p = data[i];
       var x = [p.pos.coordinate.longitude, p.pos.coordinate.latitude];
       mapGlobal.trackFeatures.push(new Feature({
         type: "track",
         geometry: new Circle(x),
       }))
       if ( i > 0 ) {
         mapGlobal.trackFeatures.push(new Feature({
           type: "track",
           geometry: new LineString([x, prev]),
         }))
       }
       prev = x;
     }

     mapGlobal.trackFeaturesLastCheck = new Date();
   }

   for ( var g in globalData.gauges ) {
     var h = globalData.gaugesToHistorical[g];
     if (h && (new Date() - h.ts) < 60000) {
       continue;
     }


     var timeStart = new Date();
     var data = await getDataViaMQL(dc, g, startTime);
     //var data = await getDataViaRaw(dc, robotName, g, startTime);
     var getDataTime = (new Date()).getTime() - timeStart.getTime();
     
     console.log("time to get graph data for " + g + " took " + getDataTime + " and had " + data.length + " points");
     
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
     res[dd._id] = dd.min;
   }
   return res;
 }
</script>


<main class="w-dvw lg:h-dvh p-2 grid grid-cols-1 lg:grid-cols-4 grid-rows-3 lg:grid-rows-6 gap-2">
  <div class="relative lg:col-span-3 row-span-3 lg:row-span-5 border border-light">
    <div id="map" class="min-h-[50dvh] h-fit"></div>
    <div class="absolute bottom-0 right-0 left-0 flex gap-4 w-full bg-white/65 p-4">
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

  <aside class="lg:row-span-6 flex flex-col gap-4 border border-light p-1 bg-white min-h-full">
    {#if globalData.status === "Connected"}
      <div class="flex gap-2 items-center w-full min-h-6 px-2 border border-success-medium bg-success-light">
        <PrimeIcon
          name="broadcast"
          cx="text-success-dark"
        />
        <div class="text-sm text-success-dark">{globalData.status}</div>
      </div>
    {:else}
    <div class="flex gap-2 items-center w-full min-h-6 px-2 border border-info-medium bg-info-light">
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
          <div class="min-w-32">Speed</div>
          <div>
            <span class="font-bold">{globalData.speed.toFixed(2)}</span>
            <sup>kn</sup>  
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
        <span class="font-bold">{@html globalData.pos.format(gpsFormatter)}</span>
      </div>
      {#if globalConfig.movementSensorProps.compassHeadingSupported}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Heading</div>
          <div>
            <span class="font-bold">{@html globalData.heading}</span>
          </div>
        </div>
      {/if}
      <div class="flex flex-col divide-y">
        {#each gaugesToArray(globalData.gauges) as [key, value]}
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
                <div role="article" tabindex="-1" class="peer bg-light hover:cursor-pointer">
                  <LinkedChart
                    data={gauageHistoricalToLinkedChart(globalData.gaugesToHistorical[key])}
                    style="width: 100%;"
                    width="100"
                    type="line"
                    scaleMax=100
                    linked="{key}"
                    uid="{key}"
                    barMinWidth="1"
                    grow
                  />
                </div>
                <div
                  class="hidden peer-hover:block z-10 text-nowrap -bottom-8 right-1 absolute border border-medium bg-white px-2 w-fit"
                >
                  <LinkedValue uid="{key}" />
                  <LinkedLabel linked="{key}"/>
                </div>
              </div>
            {/if}   
            </div>
          </section>
        {/each}
      </div>
    </div>

    <div class="grow text-xs flex flex-col flex-col-reverse text-gray-500 text-right">{globalData.numUpdates}</div>
  </aside>

  <div class="h-[50dvh] lg:h-[auto] overflow-x-auto flex lg:col-span-3 border border-light p-1 bg-white">
    {#each globalData.cameraNames as name, index}
      <img id="{name}" class="w-full lg:w-[250px]" alt="{name}" />
    {/each}
  </div>
</main>
