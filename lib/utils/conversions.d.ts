/**
 * Convert meters per second to knots
 * Used for displaying boat speed in nautical units
 */
export declare function msToKnots(speedMs: number): number;
/**
 * Calculate point difference for pan detection
 * Used to detect when user is manually panning the map
 */
export declare function pointDiff(x: number[], y: number[]): number;
/**
 * Convert Celsius to Fahrenheit
 * Used for water temperature display
 */
export declare function celsiusToFahrenheit(celsius: number): number;
/**
 * Convert liters to gallons
 * Used for fuel and water tank displays
 */
export declare function litersToGallons(liters: number): number;
/**
 * Calculate total fuel level across multiple tanks
 * Returns total fuel in gallons
 */
export declare function fuelTotalLevel(gauges: Record<string, any>): number;
/**
 * Calculate total fuel capacity across multiple tanks
 * Returns total capacity in gallons
 */
export declare function fuelTotalCapacity(gauges: Record<string, any>): number;
/**
 * Calculate average AC power voltage
 */
export declare function acPowerVoltAverage(data: Record<string, any>): number;
/**
 * Calculate total AC power amperage at specified voltage
 */
export declare function acPowerAmpAt(vvv: number, data: Record<string, any>): number;
