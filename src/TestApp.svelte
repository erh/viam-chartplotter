<script lang="ts">
  import MarineMap from "./marineMap.svelte";
  import type { BoatInfo } from "./lib/BoatInfo";
  import { onMount } from "svelte";

  // Helper to generate 24-hour position history (hourly points)
  function generateTrack(
    startLat: number, startLng: number,
    endLat: number, endLng: number,
    points: number = 24
  ): { lat: number; lng: number }[] {
    const track = [];
    for (let i = 0; i < points; i++) {
      const t = i / (points - 1);
      track.push({
        lat: startLat + (endLat - startLat) * t,
        lng: startLng + (endLng - startLng) * t,
      });
    }
    return track;
  }

  // My boat - Florida coast
  let mockMyBoat = $state<BoatInfo>({
    name: "My Boat",
    location: [26.7759, -80.0523], // Port of Palm Beach
    speed: 8.5,
    heading: 90,
  });

  // Mock AIS boats with realistic speeds and 24-hour tracks
  let mockBoats = $state<BoatInfo[]>([
    {
      name: "Gulf Runner",
      mmsi: "123456789",
      location: [26.0, -82.0], // Tampa Bay area
      speed: 8.5,
      heading: 270,
      positionHistory: generateTrack(26.05, -81.65, 26.0, -82.0, 24),
    },
    {
      name: "Keys Cruiser",
      mmsi: "987654321",
      location: [24.5, -81.8], // Key West
      speed: 5.2,
      heading: 180,
      positionHistory: generateTrack(24.7, -81.75, 24.5, -81.8, 24),
    },
    {
      name: "Atlantic Voyager",
      mmsi: "456789123",
      location: [27.0, -78.0], // East of Palm Beach, Atlantic
      speed: 12.0,
      heading: 45,
      positionHistory: generateTrack(26.6, -78.4, 27.0, -78.0, 24),
    },
    {
      name: "Miami Express",
      mmsi: "111222333",
      location: [25.8, -80.1], // Miami
      speed: 6.0,
      heading: 135,
      positionHistory: generateTrack(25.95, -80.25, 25.8, -80.1, 24),
    },
    {
      name: "Ocean Freighter",
      mmsi: "444555666",
      location: [26.5, -77.5], // Further east in Atlantic
      speed: 14.0,
      heading: 0,
      positionHistory: generateTrack(25.9, -77.5, 26.5, -77.5, 24),
    },
    {
      name: "Bermuda Runner",
      mmsi: "777888999",
      location: [28.0, -76.0], // Way out east toward Bermuda
      speed: 15.0,
      heading: 60,
      positionHistory: generateTrack(27.5, -76.5, 28.0, -76.0, 24),
    },
    {
      name: "Cape Canaveral",
      mmsi: "333444555",
      location: [28.4, -80.5], // Cape Canaveral
      speed: 3.5,
      heading: 0,
      positionHistory: generateTrack(28.25, -80.52, 28.4, -80.5, 24),
    },
    {
      name: "Gulf Stream Rider",
      mmsi: "666777888",
      location: [25.5, -79.0], // In the Gulf Stream east of Miami
      speed: 10.0,
      heading: 350,
      positionHistory: generateTrack(25.1, -79.15, 25.5, -79.0, 24),
    },
    {
      name: "Pacific Wanderer",
      mmsi: "999000111",
      location: [35.6, 139.7], // Tokyo Bay, Japan
      speed: 11.0,
      heading: 225,
      positionHistory: generateTrack(35.8, 139.9, 35.6, 139.7, 24),
    },
  ]);

  // Mock historical track for my boat (24 hours)
  const mockPositionHistorical = generateTrack(26.72, -80.08, 26.7759, -80.0523, 24);

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

      // Update my boat too
      const mySpeedFactor = mockMyBoat.speed * 0.0001;
      const myHeadingRad = (mockMyBoat.heading * Math.PI) / 180;
      mockMyBoat = {
        ...mockMyBoat,
        location: [
          mockMyBoat.location[0] + Math.cos(myHeadingRad) * mySpeedFactor,
          mockMyBoat.location[1] + Math.sin(myHeadingRad) * mySpeedFactor,
        ] as [number, number],
      };
    }, 2000);

    return () => clearInterval(interval);
  });
</script>

<div class="map-wrapper">
  <MarineMap 
    boats={mockBoats} 
    zoomModifier={-8}
    enableBoatsPanel={true}
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
