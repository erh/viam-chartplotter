/**
 * Convert dictionary to sorted array of key-value pairs
 * Used for rendering gauge data in sorted order
 */
export function dicToArray(d, sortFunction) {
    let names = Object.keys(d);
    if (sortFunction) {
        names = sortFunction(names);
    }
    else {
        names.sort();
    }
    const a = [];
    for (let i = 0; i < names.length; i++) {
        const n = names[i];
        a.push([n, d[n]]);
    }
    return a;
}
/**
 * Check if there's more than one fuel tank
 * Used to determine whether to show total fuel display
 */
export function moreThanOneFuelTank(gauges) {
    let found = false;
    for (const k in gauges) {
        const g = gauges[k];
        if (g["Type"] === "Fuel") {
            if (found) {
                return true;
            }
            found = true;
        }
    }
    return false;
}
/**
 * Convert gauge historical data to format expected by LinkedChart
 */
export function gauageHistoricalToLinkedChart(data) {
    const res = {};
    for (const d in data.data) {
        const dd = data.data[d];
        res[dd._id] = Math.floor(dd.min);
    }
    return res;
}
