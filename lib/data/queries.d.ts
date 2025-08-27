/**
 * Get gauge data via MQL query
 * Retrieves historical gauge readings aggregated in 15-minute buckets
 */
export declare function getDataViaMQL(dc: any, g: string, startTime: Date, cloudMetaData: any): Promise<any>;
/**
 * Get position history via MQL query
 * Tries configured sensor first, then falls back to alternatives
 */
export declare function positionHistoryMQL(dc: any, startTime: Date, globalConfig: any, cloudMetaData: any): Promise<any>;
/**
 * Get position history for a specific movement sensor
 * Queries position data aggregated by minute
 */
export declare function positionHistoryMQLNamed(dc: any, startTime: Date, sensorName: string, cloudMetaData: any, hot: boolean): Promise<any>;
