import * as VIAM from '@viamrobotics/sdk';

/**
 * Filter resources by type, subtype, and optional name pattern
 * Returns matching resources from the provided list
 */
export function filterResources(resources: any[], type: string, subtype: string, namePattern: RegExp | null = null) {
  const filtered = [];
  for (const resource of resources) {
    if (type !== "" && resource.type !== type) {
      continue;
    }

    if (subtype !== "" && resource.subtype !== subtype) {
      continue;
    }

    if (namePattern != null && !resource.name.match(namePattern)) {
      continue;
    }

    filtered.push(resource);
  }

  return filtered;
}

/**
 * Filter resources and return the first matching name
 * Returns empty string if no matches found
 */
export function filterResourcesFirstMatchingName(resources: any[], type: string, subtype: string, namePattern: RegExp | null = null): string {
  const matching = filterResources(resources, type, subtype, namePattern);
  if (matching.length > 0) {
    return matching[0].name;
  }
  return "";
}

/**
 * Filter resources and return all matching names sorted
 * Returns array of resource names
 */
export function filterResourcesAllMatchingNames(resources: any[], type: string, subtype: string, namePattern: RegExp | null = null): string[] {
  const matching = filterResources(resources, type, subtype, namePattern);
  const names = [];
  for (const resource of matching) {
    names.push(resource.name);
  }
  return names.sort();
}

/**
 * Setup movement sensor by finding the best available sensor
 * Prioritizes sensors with more capabilities (position, velocity, compass)
 */
export async function setupMovementSensor(client: VIAM.RobotClient, resources: any[]) {
  const movementSensors = filterResources(resources, "component", "movement_sensor", null);

  const allGpsNames = [];
  
  // Pick best movement sensor
  let bestName = "";
  let bestScore = 0;
  let bestProp = {};
  
  for (const resource of movementSensors) {
    const msClient = new VIAM.MovementSensorClient(client, resource.name);
    const prop = await msClient.getProperties();

    let score = 0;
    if (prop.positionSupported) {
      allGpsNames.push(resource.name);
      score++;
    }
    if (prop.linearVelocitySupported) {
      score++;
    }
    if (prop.compassHeadingSupported) {
      score++;
    }

    // Prefer higher scores, tie-break with shorter names
    if (score > bestScore || (score === bestScore && resource.name.length < bestName.length)) {
      bestName = resource.name;
      bestScore = score;
      bestProp = prop;
    }
  }
  
  return {
    movementSensorName: bestName,
    movementSensorProps: bestProp,
    movementSensorAlternates: allGpsNames
  };
}

/**
 * Discover and configure all sensor names from resources
 * Returns configuration object with sensor names for different types
 */
export function discoverSensorNames(resources: any[]) {
  return {
    aisSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /\bais$/),
    allPgnSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /\ball.pgn$/),
    seatempSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /\bseatemp$/),
    depthSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /depth/),
    windSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /wind/),
    spotZeroFWSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /spotzero-fw/),
    spotZeroSWSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /spotzero-sw/),
    seakeeperSensorName: filterResourcesFirstMatchingName(resources, "component", "sensor", /seakeeper/),
    acPowers: filterResourcesAllMatchingNames(resources, "component", "sensor", /\bac-\d-\d$/)
  };
}