<script lang="ts">
  import MarineMap from "./marineMap.svelte";
  import type { BoatInfo } from "./lib/BoatInfo";
  import { onMount } from "svelte";

  // Map API reference
  let mapApi = $state<{ fitToVisibleBoats: () => void }>();

  // Helper to generate position history with realistic variation
  // Generates tracks that show actual voyage patterns
  function generateTrack(
    startLat: number, startLng: number,
    endLat: number, endLng: number,
    points: number = 360
  ): { lat: number; lng: number }[] {
    const track = [];
    
    // Vector from start to end
    const dLat = endLat - startLat;
    const dLng = endLng - startLng;
    
    // Perpendicular vector for lateral movement
    const perpLat = -dLng;
    const perpLng = dLat;
    
    for (let i = 0; i < points; i++) {
      const t = i / (points - 1);
      
      // Create a winding path using superposition of sine waves
      // This creates a more natural "wandering" look than a single sine wave
      const lateral = (Math.sin(t * Math.PI * 3) * 0.05 + Math.sin(t * Math.PI * 7) * 0.02);
      
      // Small random jitter (approx 0.002 degrees ~ 200m) to simulate GPS noise/minor corrections
      const jitterLat = (Math.random() - 0.5) * 0.002;
      const jitterLng = (Math.random() - 0.5) * 0.002;
      
      const currentLat = startLat + dLat * t + perpLat * lateral + jitterLat;
      const currentLng = startLng + dLng * t + perpLng * lateral + jitterLng;
      
      track.push({
        lat: currentLat,
        lng: currentLng,
      });
    }
    return track;
  }

  // =============================================================
  // MY BOAT - Simulates what viam-chartplotter's App.svelte does:
  // A single connected machine with GPS, sensors, and live tracking
  // =============================================================
  let myBoat = $state<BoatInfo>({
    name: "My Vessel",
    location: [25.77, -80.18], // Starting position: Miami
    speed: 7.5,
    heading: 45,
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
  let myBoatHistory = $state<{ lat: number; lng: number }[]>(
    generateTrack(26.1, -80.1, 25.77, -80.18, 150) // Fort Lauderdale to current position
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
      location: [26.0, -82.0], // Tampa Bay area
      speed: 8.5,
      heading: 270,
      host: "gulf-runner.viam.cloud",
      partId: "gulf-runner-part",
      isOnline: true,
      // Voyage from East Florida coast to Tampa Bay with data gap offshore
      positionHistory: [
        ...generateTrack(27.8, -80.2, 27.0, -81.0, 100), // East coast to offshore
        // Gap: boat went offline crossing to west coast
        ...generateTrack(26.5, -82.2, 26.0, -82.0, 100), // Approaching Tampa
      ],
    },
    {
      name: "Keys Cruiser",
      mmsi: "987654321",
      location: [24.5, -81.8], // Key West
      speed: 5.2,
      heading: 180,
      host: "keys-cruiser.viam.cloud",
      partId: "keys-cruiser-part",
      isOnline: true,
      // Complete voyage down the Florida Keys (no gaps)
      positionHistory: generateTrack(26.2, -80.1, 24.5, -81.8, 300),
    },
    {
      name: "Atlantic Voyager",
      mmsi: "456789123",
      location: [27.0, -78.0], // East of Palm Beach, Atlantic
      speed: 12.0,
      heading: 45,
      host: "atlantic-voyager.viam.cloud",
      partId: "atlantic-voyager-part",
      isOnline: false, // Offline boat - like kongsberg-apps handles
      // Atlantic crossing with multiple offline periods
      positionHistory: [
        ...generateTrack(25.8, -80.1, 26.5, -79.5, 80), // Coastal departure
        // Gap: offshore
        ...generateTrack(26.8, -78.5, 27.0, -78.0, 80), // Mid-Atlantic
      ],
    },
    {
      name: "Miami Express",
      mmsi: "111222333",
      location: [25.8, -80.1], // Miami
      speed: 6.0,
      heading: 135,
      host: "miami-express.viam.cloud",
      partId: "miami-express-part",
      isOnline: true,
      // Coastal route from Fort Lauderdale to Miami (continuous track)
      positionHistory: generateTrack(26.5, -80.05, 25.8, -80.1, 200),
    },
    {
      name: "Ocean Freighter",
      mmsi: "444555666",
      location: [26.5, -77.5], // Further east in Atlantic
      speed: 14.0,
      heading: 0,
      host: "ocean-freighter.viam.cloud",
      partId: "ocean-freighter-part",
      isOnline: true,
      // Deep ocean voyage with significant data gaps
      positionHistory: [
        ...generateTrack(24.5, -78.5, 25.0, -78.0, 60),
        // Large gap in deep ocean
        ...generateTrack(26.0, -77.8, 26.5, -77.5, 60),
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
