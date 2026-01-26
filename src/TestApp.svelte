<script lang="ts">
  import MarineMap from "./marineMap.svelte";
  import type { BoatInfo } from "./lib/BoatInfo";
  import { onMount } from "svelte";

  // Map API reference
  let mapApi = $state<{ fitToVisibleBoats: () => void }>();

  // Helper to generate position history with realistic variation
  // Generates tracks that follow waypoints through water
  function generateTrackWithWaypoints(
    waypoints: { lat: number; lng: number }[],
    pointsPerSegment: number = 50
  ): { lat: number; lng: number }[] {
    const track: { lat: number; lng: number }[] = [];
    
    for (let w = 0; w < waypoints.length - 1; w++) {
      const start = waypoints[w];
      const end = waypoints[w + 1];
      const segmentPoints = w === waypoints.length - 2 ? pointsPerSegment : pointsPerSegment - 1;
      
      for (let i = 0; i < segmentPoints; i++) {
        const t = i / (pointsPerSegment - 1);
        
        // Small lateral variation for realism
        const lateral = Math.sin(t * Math.PI * 2) * 0.01 + Math.sin(t * Math.PI * 5) * 0.005;
        
        // GPS jitter
        const jitterLat = (Math.random() - 0.5) * 0.001;
        const jitterLng = (Math.random() - 0.5) * 0.001;
        
        const dLat = end.lat - start.lat;
        const dLng = end.lng - start.lng;
        
        track.push({
          lat: start.lat + dLat * t + (-dLng) * lateral + jitterLat,
          lng: start.lng + dLng * t + dLat * lateral + jitterLng,
        });
      }
    }
    return track;
  }

  // Simple two-point track generator
  function generateTrack(
    startLat: number, startLng: number,
    endLat: number, endLng: number,
    points: number = 100
  ): { lat: number; lng: number }[] {
    return generateTrackWithWaypoints([
      { lat: startLat, lng: startLng },
      { lat: endLat, lng: endLng }
    ], points);
  }

  // =============================================================
  // MY BOAT - Simulates what viam-chartplotter's App.svelte does:
  // A single connected machine with GPS, sensors, and live tracking
  // =============================================================
  let myBoat = $state<BoatInfo>({
    name: "My Vessel",
    location: [25.77, -80.18], // Starting position: Miami
    speed: 7.5,
    heading: 90,
    host: "mock-host.viam.cloud",
    partId: "my-boat-part-id",
    isOnline: true,
    // Route destination (like viam-chartplotter shows navigation)
    route: {
      destinationLatitude: 25.05,
      destinationLongitude: -77.35,
    },
  });

  // Historical track for my boat (simulates position history from movement sensor)
  // Route: Coming down the ICW / offshore from Palm Beach area to Miami
  let myBoatHistory = $state<{ lat: number; lng: number }[]>(
    generateTrackWithWaypoints([
      { lat: 26.7, lng: -80.03 },  // Offshore Palm Beach
      { lat: 26.35, lng: -80.05 }, // Offshore Boca Raton  
      { lat: 26.1, lng: -80.08 },  // Offshore Fort Lauderdale
      { lat: 25.9, lng: -80.1 },   // Offshore Hollywood
      { lat: 25.77, lng: -80.13 }, // Current position near Miami
    ], 40)
  );

  // =============================================================
  // FLEET BOATS - Simulates what kongsberg-apps fleet-chartplotter does:
  // Multiple boats from different locations, each with track history
  // from data queries (30-day position history)
  // =============================================================
  let fleetBoats = $state<BoatInfo[]>([
    {
      name: "Gulf Runner",
      mmsi: "123456789",
      location: [26.0, -82.5], // Tampa Bay area (in the Gulf)
      speed: 8.5,
      heading: 270,
      host: "gulf-runner.viam.cloud",
      partId: "gulf-runner-part",
      isOnline: true,
      // Voyage around Florida Keys to Tampa Bay (stays in water)
      positionHistory: [
        ...generateTrackWithWaypoints([
          { lat: 25.0, lng: -80.3 },   // South of Miami, offshore
          { lat: 24.7, lng: -80.8 },   // Upper Keys
          { lat: 24.55, lng: -81.5 },  // Middle Keys
          { lat: 24.55, lng: -82.0 },  // Near Key West
        ], 50),
        // Gap: went around Florida
        ...generateTrackWithWaypoints([
          { lat: 25.5, lng: -83.0 },   // In the Gulf
          { lat: 26.0, lng: -82.5 },   // Tampa Bay entrance
        ], 50),
      ],
    },
    {
      name: "Keys Cruiser",
      mmsi: "987654321",
      location: [24.55, -81.78], // Key West
      speed: 5.2,
      heading: 180,
      host: "keys-cruiser.viam.cloud",
      partId: "keys-cruiser-part",
      isOnline: true,
      // Voyage down the Florida Keys (stays in the water, follows the Keys)
      positionHistory: generateTrackWithWaypoints([
        { lat: 25.85, lng: -80.12 },  // Miami offshore
        { lat: 25.4, lng: -80.15 },   // South Miami offshore
        { lat: 25.15, lng: -80.3 },   // Key Largo area
        { lat: 24.95, lng: -80.5 },   // Upper Keys
        { lat: 24.75, lng: -80.85 },  // Islamorada area
        { lat: 24.65, lng: -81.2 },   // Marathon area
        { lat: 24.55, lng: -81.78 },  // Key West
      ], 50),
    },
    {
      name: "Atlantic Voyager",
      mmsi: "456789123",
      location: [25.3, -77.8], // Bahamas (west of Nassau)
      speed: 12.0,
      heading: 120,
      host: "atlantic-voyager.viam.cloud",
      partId: "atlantic-voyager-part",
      isOnline: false, // Offline boat
      // Crossing from Miami to Bahamas
      positionHistory: [
        ...generateTrackWithWaypoints([
          { lat: 25.78, lng: -80.1 },  // Miami departure
          { lat: 25.7, lng: -79.5 },   // Offshore heading east
          { lat: 25.5, lng: -79.0 },   // Mid-crossing
        ], 40),
        // Gap: offshore communication lost
        ...generateTrackWithWaypoints([
          { lat: 25.35, lng: -78.2 },  // Approaching Bahamas
          { lat: 25.3, lng: -77.8 },   // Current position
        ], 40),
      ],
    },
    {
      name: "Miami Express",
      mmsi: "111222333",
      location: [25.78, -80.08], // Miami Port
      speed: 6.0,
      heading: 135,
      host: "miami-express.viam.cloud",
      partId: "miami-express-part",
      isOnline: true,
      // Coastal route from Port Everglades to Miami (offshore)
      positionHistory: generateTrackWithWaypoints([
        { lat: 26.09, lng: -80.08 },   // Port Everglades
        { lat: 25.95, lng: -80.1 },    // Hollywood offshore
        { lat: 25.85, lng: -80.1 },    // Hallandale offshore
        { lat: 25.78, lng: -80.08 },   // Miami Port
      ], 60),
    },
    {
      name: "Ocean Freighter",
      mmsi: "444555666",
      location: [25.05, -77.3], // Near Nassau, Bahamas
      speed: 14.0,
      heading: 90,
      host: "ocean-freighter.viam.cloud",
      partId: "ocean-freighter-part",
      isOnline: true,
      // Deep ocean voyage staying east of Bahamas islands
      positionHistory: [
        ...generateTrackWithWaypoints([
          { lat: 23.5, lng: -75.5 },   // South, in deep Atlantic
          { lat: 24.0, lng: -76.0 },   // Heading northwest in open water
          { lat: 24.5, lng: -76.5 },   // East of Exumas
        ], 40),
        // Gap in deep ocean
        ...generateTrackWithWaypoints([
          { lat: 24.9, lng: -77.0 },   // Approaching from east
          { lat: 25.05, lng: -77.3 },  // Near Nassau (in the channel)
        ], 40),
      ],
    },
  ]);

  // Animate boats - update positions every 2 seconds
  onMount(() => {
    const interval = setInterval(() => {
      // Update my boat position (simulates live GPS updates from movement sensor)
      const mySpeedFactor = myBoat.speed / 1000;
      const myHeadingRad = (myBoat.heading * Math.PI) / 180;
      const myLatDelta = Math.cos(myHeadingRad) * mySpeedFactor;
      const myLngDelta = Math.sin(myHeadingRad) * mySpeedFactor;
      
      myBoat = {
        ...myBoat,
        location: [
          myBoat.location[0] + myLatDelta,
          myBoat.location[1] + myLngDelta,
        ] as [number, number],
        heading: (myBoat.heading + (Math.random() - 0.5) * 2 + 360) % 360,
        speed: Math.max(3, Math.min(12, myBoat.speed + (Math.random() - 0.5) * 0.5)),
      };

      // Update fleet boat positions (simulates live updates from each boat's sensors)
      fleetBoats = fleetBoats.map(boat => {
        if (boat.speed <= 1 || boat.isOnline === false) return boat; // Skip stationary or offline boats
        
        const speedFactor = boat.speed / 1000;
        const headingRad = (boat.heading * Math.PI) / 180;
        const latDelta = Math.cos(headingRad) * speedFactor;
        const lngDelta = Math.sin(headingRad) * speedFactor;
        
        // Small heading and speed variations for realism
        const headingVariation = (Math.random() - 0.5) * 1;
        const speedVariation = (Math.random() - 0.5) * 0.2;
        
        return {
          ...boat,
          location: [
            boat.location[0] + latDelta,
            boat.location[1] + lngDelta,
          ] as [number, number],
          heading: (boat.heading + headingVariation + 360) % 360,
          speed: Math.max(0, boat.speed + speedVariation),
        };
      });
    }, 2000);

    return () => clearInterval(interval);
  });
</script>

<div class="test-container">
  <div class="header">
    <h1>Chartplotter Test</h1>
    <p class="subtitle">
      Simulates both <strong>viam-chartplotter</strong> (single connected boat with live GPS) 
      and <strong>ais fleet</strong> (multiple boats with 30-day track history)
    </p>
  </div>

  <div class="map-wrapper">
    <MarineMap 
      myBoat={myBoat}
      positionHistorical={myBoatHistory}
      boats={fleetBoats} 
      zoomModifier={-8}
      enableBoatsPanel={true}
      onReady={(api) => mapApi = api}
      fitBoundsPadding={{ top: 250, right: 100, bottom: 100, left: 100 }}
      boatDetailSlot={(boat) => {
        const isMyBoat = boat.name === "My Vessel";
        const fleetBoat = fleetBoats.find(b => b.name === boat.name);
        
        return {
          render: () => `
            <div style="width: 200px; padding: 8px; background: linear-gradient(135deg, ${isMyBoat ? '#166534' : '#1e3a8a'} 0%, ${isMyBoat ? '#22c55e' : '#3b82f6'} 100%); border-radius: 4px; color: white;">
              <div style="font-size: 11px; font-weight: 600; margin-bottom: 8px; opacity: 0.9;">
                ${isMyBoat ? 'MY VESSEL' : 'FLEET VESSEL'}
              </div>
              <div style="font-size: 10px; line-height: 1.6;">
                ${isMyBoat ? `
                  <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Status:</span> <span style="color: #4ade80;">● Connected</span></div>
                  <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Destination:</span> Nassau, Bahamas</div>
                  <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">ETA:</span> ~8 hours</div>
                ` : `
                  <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Status:</span> <span style="color: ${fleetBoat?.isOnline ? '#4ade80' : '#f87171'};">${fleetBoat?.isOnline ? '● Online' : '○ Offline'}</span></div>
                  <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Type:</span> ${boat.name?.includes('Freighter') ? 'Cargo' : 'Pleasure Craft'}</div>
                  <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Track:</span> ${fleetBoat?.positionHistory?.length || 0} points</div>
                `}
                <div style="opacity: 0.7; font-size: 9px; margin-top: 8px; padding-top: 8px; border-top: 1px solid rgba(255,255,255,0.2);">
                  Last Update: ${new Date().toLocaleTimeString()}
                </div>
              </div>
            </div>
          `
        }
      }}
    />
  </div>

  <div class="legend">
    <div class="legend-item">
      <span class="legend-line" style="background: #3b82f6;"></span>
      <span>Boat Tracks</span>
    </div>
    <div class="legend-item">
      <span class="legend-line" style="background: #22c55e;"></span>
      <span>Route</span>
    </div>
  </div>
</div>

<style>
  .test-container {
    position: fixed;
    top: 0;
    left: 0;
    width: 100vw;
    height: 100vh;
    display: flex;
    flex-direction: column;
    overflow: hidden;
    background: #0f172a;
  }

  .header {
    padding: 12px 20px;
    background: rgba(15, 23, 42, 0.95);
    border-bottom: 1px solid rgba(255, 255, 255, 0.1);
    z-index: 100;
  }

  .header h1 {
    margin: 0;
    font-size: 18px;
    font-weight: 600;
    color: #f1f5f9;
  }

  .subtitle {
    margin: 4px 0 0 0;
    font-size: 12px;
    color: #94a3b8;
  }

  .subtitle strong {
    color: #60a5fa;
  }

  .map-wrapper {
    flex: 1;
    overflow: hidden;
  }

  .map-wrapper :global(#map-container) {
    width: 100%;
    height: 100%;
  }

  .map-wrapper :global(#map) {
    width: 100%;
    height: 100%;
  }

  .legend {
    position: absolute;
    bottom: 20px;
    left: 20px;
    background: rgba(15, 23, 42, 0.9);
    padding: 12px 16px;
    border-radius: 8px;
    display: flex;
    gap: 20px;
    z-index: 100;
    border: 1px solid rgba(255, 255, 255, 0.1);
  }

  .legend-item {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    color: #e2e8f0;
  }

  .legend-line {
    width: 20px;
    height: 3px;
    border-radius: 2px;
  }
</style>
