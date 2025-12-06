<script lang="ts">
  import MarineMap from "./marineMap.svelte";
  import type { BoatInfo } from "./lib/BoatInfo";
  import { onMount } from "svelte";

  // My boat - Florida coast
  let mockMyBoat = $state<BoatInfo>({
    name: "My Boat",
    location: [26.7759, -80.0523], // Port of Palm Beach
    speed: 8.5,
    heading: 281,
  });

  // Mock AIS boats - clustered around Florida/Gulf for visibility
  let mockBoats = $state<BoatInfo[]>([
    {
      name: "Gulf Runner",
      mmsi: "123456789",
      location: [26.0, -82.0], // Tampa Bay area
      speed: 12.2,
      heading: 270,
    },
    {
      name: "Keys Cruiser",
      mmsi: "987654321",
      location: [24.5, -81.8], // Key West
      speed: 6.1,
      heading: 180,
    },
    {
      name: "Atlantic Voyager",
      mmsi: "456789123",
      location: [27.0, -78.0], // East of Palm Beach, Atlantic
      speed: 9.0,
      heading: 45,
    },
    {
      name: "Miami Express",
      mmsi: "111222333",
      location: [25.8, -80.1], // Miami
      speed: 15.5,
      heading: 315,
    },
    {
      name: "Ocean Freighter",
      mmsi: "444555666",
      location: [26.5, -77.5], // Further east in Atlantic
      speed: 14.0,
      heading: 0,
    },
    {
      name: "Bermuda Runner",
      mmsi: "777888999",
      location: [28.0, -76.0], // Way out east toward Bermuda
      speed: 45.0,
      heading: 60,
    },
    {
      name: "Cape Canaveral",
      mmsi: "333444555",
      location: [28.4, -80.5], // Cape Canaveral
      speed: 4.0,
      heading: 0,
    },
    {
      name: "Gulf Stream Rider",
      mmsi: "666777888",
      location: [25.5, -79.0], // In the Gulf Stream east of Miami
      speed: 12.0,
      heading: 350,
    },
  ]);

  // Mock historical track for my boat
  const mockPositionHistorical = [
    { lat: 26.7700, lng: -80.0600 },
    { lat: 26.7720, lng: -80.0580 },
    { lat: 26.7740, lng: -80.0550 },
    { lat: 26.7759, lng: -80.0523 },
  ];

  // Animate boats - update positions every 2 seconds
  onMount(() => {
    const interval = setInterval(() => {
      // Update each AIS boat position slightly based on heading and speed
      mockBoats = mockBoats.map(boat => {
        const speedFactor = boat.speed * 0.0001; // Small movement
        const headingRad = (boat.heading * Math.PI) / 180;
        const latDelta = Math.cos(headingRad) * speedFactor;
        const lngDelta = Math.sin(headingRad) * speedFactor;
        
        return {
          ...boat,
          location: [
            boat.location[0] + latDelta,
            boat.location[1] + lngDelta,
          ] as [number, number],
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

<main class="test-app">
  <header>
    <h1>MarineMap Test - Multi-Boat View</h1>
    <p>Testing MarineMap component with {mockBoats.length} AIS boats + my boat (animated)</p>
  </header>
  
  <div class="map-wrapper">
    <MarineMap 
      myBoat={mockMyBoat} 
      boats={mockBoats} 
      zoomModifier={-8}
      positionHistorical={mockPositionHistorical}
    />
  </div>

  <div class="boat-list">
    <h2>Active Boats ({mockBoats.length + 1})</h2>
    <table>
      <thead>
        <tr>
          <th>Name</th>
          <th>MMSI</th>
          <th>Speed (kn)</th>
          <th>Heading</th>
          <th>Position</th>
        </tr>
      </thead>
      <tbody>
        <tr class="my-boat">
          <td><strong>{mockMyBoat.name}</strong></td>
          <td>-</td>
          <td>{mockMyBoat.speed.toFixed(1)}</td>
          <td>{mockMyBoat.heading}°</td>
          <td>{mockMyBoat.location[0].toFixed(4)}°N, {Math.abs(mockMyBoat.location[1]).toFixed(4)}°W</td>
        </tr>
        {#each mockBoats as boat}
          <tr>
            <td>{boat.name}</td>
            <td>{boat.mmsi}</td>
            <td>{boat.speed.toFixed(1)}</td>
            <td>{boat.heading}°</td>
            <td>{boat.location[0].toFixed(4)}°N, {Math.abs(boat.location[1]).toFixed(4)}°W</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
</main>

<style>
  .test-app {
    padding: 16px;
    font-family: system-ui, -apple-system, sans-serif;
    background: #1a1a2e;
    color: #e0e0e0;
    min-height: 100vh;
    max-width: 100%;
    box-sizing: border-box;
  }

  header {
    margin-bottom: 12px;
  }

  h1, h2 {
    color: #fff;
    margin-bottom: 4px;
  }

  h1 {
    font-size: 1.5rem;
  }

  p {
    color: #888;
    margin-bottom: 0;
    font-size: 0.9rem;
  }

  .map-wrapper {
    width: 100%;
    aspect-ratio: 21 / 9;
    border: 1px solid #333;
    border-radius: 8px;
    overflow: hidden;
    position: relative;
    margin-bottom: 20px;
  }

  .map-wrapper :global(#map-container) {
    width: 100%;
    height: 100%;
  }

  .map-wrapper :global(#map) {
    width: 100%;
    height: 100%;
  }

  .boat-list {
    margin-top: 0;
  }

  .boat-list h2 {
    margin-bottom: 12px;
  }

  table {
    width: 100%;
    border-collapse: collapse;
    background: #16213e;
    border: 1px solid #333;
    border-radius: 4px;
  }

  th, td {
    padding: 10px 14px;
    text-align: left;
    border-bottom: 1px solid #333;
  }

  th {
    background: #0f3460;
    font-weight: 600;
    color: #e0e0e0;
    border-bottom: 2px solid #444;
  }

  td {
    color: #ccc;
  }

  tr:hover {
    background: #1f4068;
  }

  .my-boat {
    background: #1a4d6e;
  }

  .my-boat:hover {
    background: #236690;
  }
</style>
