import * as VIAM from '@viamrobotics/sdk';
/**
 * Filter resources by type, subtype, and optional name pattern
 * Returns matching resources from the provided list
 */
export declare function filterResources(resources: any[], type: string, subtype: string, namePattern?: RegExp | null): any[];
/**
 * Filter resources and return the first matching name
 * Returns empty string if no matches found
 */
export declare function filterResourcesFirstMatchingName(resources: any[], type: string, subtype: string, namePattern?: RegExp | null): string;
/**
 * Filter resources and return all matching names sorted
 * Returns array of resource names
 */
export declare function filterResourcesAllMatchingNames(resources: any[], type: string, subtype: string, namePattern?: RegExp | null): string[];
/**
 * Setup movement sensor by finding the best available sensor
 * Prioritizes sensors with more capabilities (position, velocity, compass)
 */
export declare function setupMovementSensor(client: VIAM.RobotClient, resources: any[]): Promise<{
    movementSensorName: string;
    movementSensorProps: {};
    movementSensorAlternates: any[];
}>;
/**
 * Discover and configure all sensor names from resources
 * Returns configuration object with sensor names for different types
 */
export declare function discoverSensorNames(resources: any[]): {
    aisSensorName: string;
    allPgnSensorName: string;
    seatempSensorName: string;
    depthSensorName: string;
    windSensorName: string;
    spotZeroFWSensorName: string;
    spotZeroSWSensorName: string;
    seakeeperSensorName: string;
    acPowers: string[];
};
