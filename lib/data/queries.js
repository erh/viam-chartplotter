import { BSON } from "bsonfy";
/**
 * Get gauge data via MQL query
 * Retrieves historical gauge readings aggregated in 15-minute buckets
 */
export async function getDataViaMQL(dc, g, startTime, cloudMetaData) {
    const match = {
        "location_id": cloudMetaData.locationId,
        "robot_id": cloudMetaData.machineId,
        "component_name": g,
        time_received: { $gte: startTime }
    };
    const group = {
        "_id": {
            "$concat": [
                { "$toString": { "$substr": [{ "$year": "$time_received" }, 2, -1] } },
                "-",
                { "$toString": { "$month": "$time_received" } },
                "-",
                { "$toString": { "$dayOfMonth": "$time_received" } },
                " ",
                { "$toString": { "$hour": "$time_received" } },
                ":",
                { "$toString": { "$multiply": [15, { "$floor": { "$divide": [{ "$minute": "$time_received" }, 15] } }] } }
            ]
        },
        "ts": { "$min": "$time_received" },
        "min": { "$min": "$data.readings.Level" },
        "max": { "$max": "$data.readings.Level" }
    };
    const query = [
        BSON.serialize({ "$match": match }),
        BSON.serialize({ "$group": group }),
        BSON.serialize({ "$sort": { ts: -1 } }),
        BSON.serialize({ "$limit": (24 * 4) }),
        BSON.serialize({ "$sort": { ts: 1 } }),
    ];
    const data = await dc.tabularDataByMQL(cloudMetaData.primaryOrgId, query, true);
    return data;
}
/**
 * Get position history via MQL query
 * Tries configured sensor first, then falls back to alternatives
 */
export async function positionHistoryMQL(dc, startTime, globalConfig, cloudMetaData) {
    if (globalConfig.movementSensorForQuery !== "") {
        const res = await positionHistoryMQLNamed(dc, startTime, globalConfig.movementSensorForQuery, cloudMetaData, false);
        if (res.length > 0) {
            return res;
        }
    }
    for (let i = 0; i < globalConfig.movementSensorAlternates.length; i++) {
        const n = globalConfig.movementSensorAlternates[i];
        const res = await positionHistoryMQLNamed(dc, startTime, n, cloudMetaData, false);
        if (res.length > 0) {
            globalConfig.movementSensorForQuery = n;
            return res;
        }
    }
    return [];
}
/**
 * Get position history for a specific movement sensor
 * Queries position data aggregated by minute
 */
export async function positionHistoryMQLNamed(dc, startTime, sensorName, cloudMetaData, hot) {
    const name = sensorName.split(":");
    const match = {
        "location_id": cloudMetaData.locationId,
        "robot_id": cloudMetaData.machineId,
        "component_name": name[name.length - 1],
        "method_name": "Position",
        "time_received": { $gte: startTime }
    };
    const group = {
        "_id": {
            "$concat": [
                { "$toString": { "$substr": [{ "$year": "$time_received" }, 2, -1] } },
                "-",
                { "$toString": { "$month": "$time_received" } },
                "-",
                { "$toString": { "$dayOfMonth": "$time_received" } },
                " ",
                { "$toString": { "$hour": "$time_received" } },
                ":",
                { "$toString": { "$minute": "$time_received" } },
            ]
        },
        "ts": { "$min": "$time_received" },
        "pos": { "$first": "$data" },
    };
    const query = [
        BSON.serialize({ "$match": match }),
        BSON.serialize({ "$sort": { ts: -1 } }),
        BSON.serialize({ "$group": group }),
        BSON.serialize({ "$sort": { ts: -1 } }),
    ];
    const timeStart = new Date();
    const data = await dc.tabularDataByMQL(cloudMetaData.primaryOrgId, query, hot);
    const getDataTime = (new Date()).getTime() - timeStart.getTime();
    console.log("got " + data.length + " history data points from:" + sensorName + " in " + getDataTime + "ms");
    return data;
}
