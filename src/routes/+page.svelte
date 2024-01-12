<script lang="ts">
 import '../app.css'
 import { onMount } from 'svelte';
 import * as VIAM from '@viamrobotics/sdk';
 
 let client: VIAM.RobotClient;
 let pos = {};
 let numUpdates = 0;
 let speed = 0.0;
 let temp = 0.0;
 let status = "not connected yet";

 function errorHandler(e) {
   console.log("error: ", e);
 }
 
 function doUpdate(){
   if (!client) {
     return;
   }
   
   numUpdates++;

   const msClient = new VIAM.MovementSensorClient(client, 'cm90-garmin1-main:garmin');
   
   msClient.getPosition().then((p) => {
     pos = p.coordinate;
   }).catch(errorHandler);

   msClient.getLinearVelocity().then((v) => {
     speed = v.y * 1.94384;
   }).catch(errorHandler);
   
   new VIAM.SensorClient(client, "cm90-garmin1-main:seatemp").getReadings().then((t) => {
     temp = 32 + (t.Temperature * 1.8);
   }).catch( errorHandler );
 }

 function updateAndLoop() {
   doUpdate();

   setTimeout(updateAndLoop, 1000);
 }

 async function connect(host: string, credential, authEntity: string): Promise<VIAM.RobotClient> {
   return VIAM.createRobotClient({
     host,
     credential: credential,
     authEntity: authEntity,
     signalingAddress: 'https://app.viam.com:443',

     // optional: configure reconnection options
     reconnectMaxAttempts: 7,
     reconnectMaxWait: 1000,
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
   console.log(urlParams);

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
     /*
        const streamClient = new VIAM.StreamClient(client)
        streamClient.getStream("cockpit").then( 
        function(mediaStream){
        console.log(mediaStream);
        document.getElementById("theVideo").srcObject = mediaStream;
        }
        );
      */      
     
     return { client: client, "x" : 5}
     
   } catch (error) {
     errorHandler(error);
   }

 }

 onMount(start);
</script>

<h2>{status}</h2>
<div>
  <div>
  </div>
  <div id="navData">
    <p class="data" >{speed.toFixed(2)} kts</p>
    <p class="data" >{temp.toFixed(2)} f</p>
    <p class="data" >
      lat: {pos.latitude}
      <br>
      lon: {pos.longitude}
    </p>
  </div>
</div>

<small>{numUpdates}</small>


