<script lang="ts">
 import '../app.css'
 import { onMount } from 'svelte';

 import {Coordinate} from "tsgeo/Coordinate";
 import {DMS}        from "tsgeo/Formatter/Coordinate/DMS";

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

 
 let client: VIAM.RobotClient;
 let pos = new Coordinate(0,0);
 let numUpdates = 0;
 let speed = 0.0;
 let temp = 0.0;
 let depth = 0.0;
 let status = "not connected yet";
 let map: Map = null;
 let view: View = null;
 let myBoatMarker: Feature = null;
 
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
 
 function errorHandler(e) {
   console.log("error: ", e);
 }
 
 function doUpdate(loopNumber: int){
   if (!client) {
     return;
   }
   
   const msClient = new VIAM.MovementSensorClient(client, 'cm90-garmin1-main:garmin');
   
   msClient.getPosition().then((p) => {
     pos = new Coordinate(p.coordinate.latitude, p.coordinate.longitude);
     
     var sz = map.getSize();
     var pp = [pos.lng, pos.lat];
     view.centerOn(pp, map.getSize(), [sz[0]/2,sz[1]/2]);

     myBoatMarker.setGeometry(new Point(pp));
     
   }).catch(errorHandler);

   msClient.getLinearVelocity().then((v) => {
     speed = v.y * 1.94384;

     // zoom of 10 is about 30 miles
     // zoom of 16 is city level

     var zoom = Math.floor(16-Math.floor(speed)^.5);
     view.setZoom(zoom);
   }).catch(errorHandler);
   
   new VIAM.SensorClient(client, "cm90-garmin1-main:seatemp").getReadings().then((t) => {
     temp = 32 + (t.Temperature * 1.8);
   }).catch( errorHandler );

   new VIAM.SensorClient(client, "cm90-garmin1-main:depth-raw").getReadings().then((d) => {
     depth = d.Depth * 3.28084;
   }).catch( errorHandler );

   
 }

 function doCameraLoop(loopNumber: int) {

   new VIAM.CameraClient(client, "cockpit").getImage().then(
     function(img){
       document.getElementById('cam1').src = URL.createObjectURL(new Blob([img]));
   }).catch(errorHandler);

   new VIAM.CameraClient(client, "enginer").getImage().then(
     function(img){
       document.getElementById('cam2').src = URL.createObjectURL(new Blob([img]));
   }).catch(errorHandler);
   
   new VIAM.CameraClient(client, "cm90-garmin1-main:flir-ffmpeg").getImage().then(
     function(img){
       document.getElementById('cam3').src = URL.createObjectURL(new Blob([img]));
   }).catch(errorHandler);

   new VIAM.CameraClient(client, "cm90-garmin1-main:flir-ffmpeg-ir").getImage().then(
     function(img){
       document.getElementById('cam4').src = URL.createObjectURL(new Blob([img]));
   }).catch(errorHandler);

 }
 
 function updateAndLoop() {
   numUpdates++;
   
   doUpdate(numUpdates);
   doCameraLoop(numUpdates);

   setTimeout(updateAndLoop, 1000);
 }

 async function connect(host: string, credential, authEntity: string): Promise<VIAM.RobotClient> {
   return VIAM.createRobotClient({
     host,
     credential: credential,
     authEntity: authEntity,
     signalingAddress: 'https://app.viam.com:443',

     // optional: configure reconnection options
     reconnectMaxAttempts: 20,
     reconnectMaxWait: 5000,
   });
 }

 async function disconnected(event) {
   status = "disconnected";
   console.log('The robot has been disconnected. Trying reconnect...');
 }

 async function reconnected(event) {
   status = "connected";
   console.log('The robot has been reconnected. Work can be continued.');
 }


 async function start() {

   const urlParams = new URLSearchParams(window.location.search);

   const host = urlParams.get("host");
   const apiKey = urlParams.get("api-key");
   const authEntity = urlParams.get("authEntity");

   const credential = {
     type: 'api-key',
     payload: apiKey
   };
   
   try {
     client = await connect(host, credential, authEntity);
     status = "connected";
     console.log('connected!');
     
     client.on('disconnected', disconnected);
     client.on('reconnected', reconnected);
     
     updateAndLoop();

     setupMap();
     
     return { client: client, "x" : 5}
     
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
   if (false) {
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
   if (false) {
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
     geometry: new Point([0,0]),
   });
   
   const vectorLayer = new Vector({
     source: new VectorSource({
       features: [myBoatMarker],
     }),
     style: function (feature) {
       return new Style({
         image: new CircleStyle({
           radius: 7,
           fill: new Fill({color: 'black'}),
           stroke: new Stroke({
             color: 'white',
             width: 2,
           }),
         })
       })
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
</script>


<div>
  <table border="1">
    <tr>
      <th colspan="2">{status}</th>
    </tr>
    <tr>
      <td>
        <div id="map"></div>
      </td>
      <td id="navData">
        <div class="data" >
          <div>Speed knots</div>
          {speed.toFixed(2)}
        </div>
        <div class="data" >
          <div>Depth ft</div>
          {depth.toFixed(2)}
        </div>
        <div class="data" >
          <div>Water Temp (f)</div>
          {temp.toFixed(2)} f
        </div>
        <div class="data" >
          <div>Location</div>
          {@html pos.format(gpsFormatter)}
        </div>
      </td>
    </tr>
    <tr>
      <td colspan="2">
        <img id="cam1" width="250"/>
        <img id="cam2" width="250"/>
        <img id="cam3" width="250"/>
        <img id="cam4" width="250"/>
      </td>
    </tr>
  </table>
</div>
        
<small>{numUpdates}</small>


