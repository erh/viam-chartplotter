/**
 * Convert meters per second to knots
 * Used for displaying boat speed in nautical units
 */
export function msToKnots(speedMs) {
    return speedMs * 1.94384;
}
/**
 * Calculate point difference for pan detection
 * Used to detect when user is manually panning the map
 */
export function pointDiff(x, y) {
    const a = x[0] - y[0];
    const b = x[1] - y[1];
    const c = a * a + b * b;
    return Math.sqrt(c);
}
/**
 * Convert Celsius to Fahrenheit
 * Used for water temperature display
 */
export function celsiusToFahrenheit(celsius) {
    return 32 + (celsius * 1.8);
}
/**
 * Convert liters to gallons
 * Used for fuel and water tank displays
 */
export function litersToGallons(liters) {
    return liters * 0.264172;
}
/**
 * Calculate total fuel level across multiple tanks
 * Returns total fuel in gallons
 */
export function fuelTotalLevel(gauges) {
    let total = 0;
    for (const k in gauges) {
        const g = gauges[k];
        if (g["Type"] !== "Fuel") {
            continue;
        }
        total += g["Level"] * g["Capacity"] / 100;
    }
    return Math.round(total * 0.264172);
}
/**
 * Calculate total fuel capacity across multiple tanks
 * Returns total capacity in gallons
 */
export function fuelTotalCapacity(gauges) {
    let total = 0;
    for (const k in gauges) {
        const g = gauges[k];
        if (g["Type"] !== "Fuel") {
            continue;
        }
        total += g["Capacity"];
    }
    return Math.round(total * 0.264172);
}
/**
 * Calculate average AC power voltage
 */
export function acPowerVoltAverage(data) {
    let total = 0;
    let num = 0;
    for (const k in data) {
        const dd = data[k];
        total += dd["Line-Neutral AC RMS Voltage"];
        num++;
    }
    return total / num;
}
/**
 * Calculate total AC power amperage at specified voltage
 */
export function acPowerAmpAt(vvv, data) {
    let total = 0;
    for (const k in data) {
        const dd = data[k];
        const a = dd["AC RMS Current"];
        const v = dd["Line-Neutral AC RMS Voltage"];
        const w = a * v;
        total += w / vvv;
    }
    return total;
}
