<script lang="ts">
  import MarineMap from "./marineMap.svelte";
  import type { BoatInfo } from "./lib/BoatInfo";
  import { onMount } from "svelte";

  // Map API reference
  let mapApi = $state<{ fitToVisibleBoats: () => void }>();

  // Helper to generate 30-day position history with realistic variation
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

  // Mock AIS boats with realistic speeds and 30-day tracks showing actual voyage patterns
  // Some boats have intentional gaps (>10nm) to demonstrate gap detection
  let mockBoats = $state<BoatInfo[]>([
    {
      name: "Gulf Runner",
      mmsi: "123456789",
      location: [26.0, -82.0], // Tampa Bay area
      speed: 8.5,
      heading: 270,
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
      // Complete voyage down the Florida Keys (no gaps)
      positionHistory: generateTrack(26.2, -80.1, 24.5, -81.8, 300),
    },
    {
      name: "Atlantic Voyager",
      mmsi: "456789123",
      location: [27.0, -78.0], // East of Palm Beach, Atlantic
      speed: 12.0,
      heading: 45,
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
      // Coastal route from Fort Lauderdale to Miami (continuous track)
      positionHistory: generateTrack(26.5, -80.05, 25.8, -80.1, 200),
    },
    {
      name: "Ocean Freighter",
      mmsi: "444555666",
      location: [26.5, -77.5], // Further east in Atlantic
      speed: 14.0,
      heading: 0,
      // Deep ocean voyage with significant data gaps
      positionHistory: [
        ...generateTrack(24.5, -78.5, 25.0, -78.0, 60),
        // Large gap in deep ocean
        ...generateTrack(26.0, -77.8, 26.5, -77.5, 60),
      ],
    },
    {
      name: "Bermuda Runner",
      mmsi: "777888999",
      location: [28.0, -76.0], // Way out east toward Bermuda
      speed: 15.0,
      heading: 60,
      // Long voyage toward Bermuda with gaps
      positionHistory: [
        ...generateTrack(26.0, -80.0, 26.8, -78.5, 80),
        // Gap crossing to Bermuda waters
        ...generateTrack(27.5, -76.8, 28.0, -76.0, 80),
      ],
    },
    {
      name: "Cape Canaveral",
      mmsi: "333444555",
      location: [28.4, -80.5], // Cape Canaveral
      speed: 3.5,
      heading: 0,
      // Local cruise around Cape Canaveral (continuous)
      positionHistory: generateTrack(28.0, -80.6, 28.4, -80.5, 150),
    },
    {
      name: "Gulf Stream Rider",
      mmsi: "666777888",
      location: [25.5, -79.0], // In the Gulf Stream east of Miami
      speed: 10.0,
      heading: 350,
      // Riding north in Gulf Stream with brief gap
      positionHistory: [
        ...generateTrack(24.0, -79.5, 24.8, -79.3, 100),
        // Brief gap
        ...generateTrack(25.2, -79.1, 25.5, -79.0, 100),
      ],
    },
  ]);

  // Animate boats - update positions every 2 seconds
  onMount(() => {
    const interval = setInterval(() => {
      // Update each AIS boat position based on heading and speed
      // Scale: 2 seconds real time ≈ 2 minutes simulated
      mockBoats = mockBoats.map(boat => {
        if (boat.speed <= 1) return boat; // Skip stationary boats
        
        const speedFactor = boat.speed / 1000; // Slower, more realistic movement
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

<div class="map-wrapper">
  <MarineMap 
    boats={mockBoats} 
    zoomModifier={-8}
    enableBoatsPanel={true}
    onReady={(api) => mapApi = api}
    fitBoundsPadding={{ top: 250, right: 100, bottom: 100, left: 100 }}
    boatDetailSlot={(boat) => {
      return {
        render: () => `
          <div style="width: 200px; padding: 8px; background: linear-gradient(135deg, #1e3a8a 0%, #3b82f6 100%); border-radius: 4px; color: white;">
            <div style="font-size: 11px; font-weight: 600; margin-bottom: 8px; opacity: 0.9;">VESSEL INFO</div>
            <div style="font-size: 10px; line-height: 1.6;">
              <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">MMSI:</span> <span style="font-family: monospace;">${boat.name?.includes('Runner') ? '369' : '367'}${Math.floor(Math.random() * 1000000).toString().padStart(6, '0')}</span></div>
              <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Type:</span> ${boat.name?.includes('Freighter') ? 'Cargo' : 'Pleasure Craft'}</div>
              <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Length:</span> ${Math.floor(Math.random() * 40) + 30}m</div>
              <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Beam:</span> ${Math.floor(Math.random() * 10) + 8}m</div>
              <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Draft:</span> ${(Math.random() * 3 + 2).toFixed(1)}m</div>
              <div style="margin-bottom: 4px;"><span style="opacity: 0.7;">Destination:</span> ${['MIAMI', 'KEY WEST', 'TAMPA', 'NASSAU', 'FREEPORT'][Math.floor(Math.random() * 5)]}</div>
              <div style="opacity: 0.7; font-size: 9px; margin-top: 8px; padding-top: 8px; border-top: 1px solid rgba(255,255,255,0.2);">Last Update: ${new Date().toLocaleTimeString()}</div>
            </div>
          </div>
        `
      }
    }}
  />
</div>

<style>
  .map-wrapper {
    position: fixed;
    top: 0;
    left: 0;
    width: 100vw;
    height: 100vh;
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
</style>
