/**
 * Convert dictionary to sorted array of key-value pairs
 * Used for rendering gauge data in sorted order
 */
export declare function dicToArray(d: Record<string, any>, sortFunction?: (names: string[]) => string[]): [string, any][];
/**
 * Check if there's more than one fuel tank
 * Used to determine whether to show total fuel display
 */
export declare function moreThanOneFuelTank(gauges: Record<string, any>): boolean;
/**
 * Convert gauge historical data to format expected by LinkedChart
 */
export declare function gauageHistoricalToLinkedChart(data: any): Record<string, number>;
