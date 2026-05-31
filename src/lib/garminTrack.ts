// garmin-position-decimated historical track.
//
// The map fires a bbox callback on first render and whenever the viewport
// settles. We (re)query the garmin-position-decimated data pipeline's sink
// collection for our own boat's track points inside that bounding box
// (2dsphere geo search on `location`, filtered by part_id) and hand the
// resulting points to MarineMap as the faded long-term track.
//
// This module holds the pure query/mapping logic; App.svelte owns the
// orchestration (debounce, in-flight guard, keyed retry, state assignment).

import * as VIAM from "@viamrobotics/sdk";
import { BSON } from "bsonfy";
import type { PositionPoint } from "./BoatInfo";

// The garmin-position-decimated data pipeline. Hardcoded id — the
// TabularDataSource API only addresses a pipeline sink by id, not name.
export const GARMIN_PIPELINE_ID = "00a6e1ab-a8b6-4501-9e15-909e15dc4fa7";

// Cap on rows pulled per viewport. The pipeline is already decimated so this
// is a safety bound, not an expected ceiling.
const DECIMATED_LIMIT = 10000;

export interface Bbox {
  minLon: number;
  minLat: number;
  maxLon: number;
  maxLat: number;
}

// Stable string key for a bbox, used to dedupe repeat queries for the same
// (rounded) viewport.
export function bboxKey(b: Bbox): string {
  return `${b.minLon},${b.minLat},${b.maxLon},${b.maxLat}`;
}

// GeoJSON polygon for the viewport (lon/lat, closed ring). 2dsphere
// $geoWithin needs a $geometry — a legacy $box would ignore the index.
function bboxToPolygon(bbox: Bbox) {
  return {
    type: "Polygon",
    coordinates: [
      [
        [bbox.minLon, bbox.minLat],
        [bbox.maxLon, bbox.minLat],
        [bbox.maxLon, bbox.maxLat],
        [bbox.minLon, bbox.maxLat],
        [bbox.minLon, bbox.minLat],
      ],
    ],
  };
}

// Map one pipeline-sink document to a PositionPoint. The collection is
// indexed on `location` (GeoJSON Point) + part_id, so coordinates come from
// location.coordinates ([lng, lat]). Timestamp field name isn't guaranteed,
// so we accept the usual candidates and tolerate its absence (the track
// renderer copes with a missing ts).
export function decimatedRowToPoint(row: any): PositionPoint | null {
  const loc = row?.location ?? row?.loc ?? row?.position;
  let lng: number | undefined;
  let lat: number | undefined;
  if (loc && Array.isArray(loc.coordinates) && loc.coordinates.length >= 2) {
    lng = loc.coordinates[0];
    lat = loc.coordinates[1];
  } else if (Array.isArray(loc) && loc.length >= 2) {
    lng = loc[0];
    lat = loc[1];
  }
  if (typeof lat !== "number" || typeof lng !== "number" || isNaN(lat) || isNaN(lng)) {
    return null;
  }
  const rawTs =
    row?.time_requested ??
    row?.time_received ??
    row?.time ??
    row?.ts ??
    row?.timestamp ??
    row?._viam_pipeline_run?.interval?.start;
  let ts: Date | undefined;
  if (rawTs instanceof Date) {
    ts = rawTs;
  } else if (typeof rawTs === "string") {
    const d = new Date(rawTs);
    if (!isNaN(d.getTime())) ts = d;
  } else if (typeof rawTs === "number") {
    ts = new Date(rawTs);
  }
  return { lat, lng, ts: ts as Date };
}

// Query the pipeline sink for our boat's decimated track inside `bbox` and
// return the points sorted ascending by timestamp. Throws on query failure
// so the caller can decide whether to retry (it leaves its fetched-key
// unset). `dataClient` is a ViamClient's dataClient.
export async function fetchGarminDecimatedTrack(
  dataClient: any,
  orgId: string,
  partId: string,
  bbox: Bbox,
  pipelineId: string = GARMIN_PIPELINE_ID
): Promise<PositionPoint[]> {
  const polygon = bboxToPolygon(bbox);
  const stages = [
    {
      $match: {
        part_id: partId,
        location: { $geoWithin: { $geometry: polygon } },
      },
    },
    { $limit: DECIMATED_LIMIT },
  ];
  const query = stages.map((s) => BSON.serialize(s));
  const dataSource = new VIAM.dataApi.TabularDataSource({
    type: VIAM.dataApi.TabularDataSourceType.PIPELINE_SINK,
    pipelineId,
  });

  // Log the exact request (pre-serialization) so the live part_id /
  // polygon / pipeline / org values for this query are inspectable.
  console.log("garmin decimated query:", {
    orgId,
    pipelineId,
    tabularDataSourceType: "PIPELINE_SINK",
    useRecentData: false,
    mql: stages,
  });

  const t0 = performance.now();
  const rows: any[] = await dataClient.tabularDataByMQL(orgId, query, false, dataSource);
  if (rows.length > 0) {
    // Surface the document shape once so field-name assumptions in
    // decimatedRowToPoint can be verified against the real pipeline.
    console.log("garmin decimated sample row:", rows[0]);
  }
  const points = rows
    .map(decimatedRowToPoint)
    .filter((p): p is PositionPoint => p !== null)
    .sort((a, b) => {
      const ta = a.ts instanceof Date ? a.ts.getTime() : Infinity;
      const tb = b.ts instanceof Date ? b.ts.getTime() : Infinity;
      return ta - tb;
    });

  // Bounds + time span help confirm the points actually fall inside the
  // current viewport (and aren't, say, all collapsed at one dock).
  const lats = points.map((p) => p.lat);
  const lngs = points.map((p) => p.lng);
  const withTs = points.filter((p) => p.ts instanceof Date);
  console.log(
    "garmin decimated track: " +
      points.length +
      "/" +
      rows.length +
      " points in " +
      Math.round(performance.now() - t0) +
      "ms" +
      (rows.length >= DECIMATED_LIMIT ? " (hit limit — track may be truncated)" : ""),
    {
      lat: lats.length ? [Math.min(...lats), Math.max(...lats)] : null,
      lng: lngs.length ? [Math.min(...lngs), Math.max(...lngs)] : null,
      withTimestamp: withTs.length,
      bbox: [bbox.minLon, bbox.minLat, bbox.maxLon, bbox.maxLat],
    }
  );

  return points;
}
