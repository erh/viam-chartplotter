import { describe, it, expect } from "vitest";
import {
  haversineMeters,
  pathLengthMeters,
  decimateByDistance,
  douglasPeucker,
  simplifyTrack,
  type LatLng,
} from "./simplify";

// One degree of latitude ≈ this many meters for R = 6371008.8 (2πR/360).
const DEG_LAT_M = 111194.93;

describe("haversineMeters", () => {
  it("is zero for identical points", () => {
    expect(haversineMeters({ lat: 40, lng: -70 }, { lat: 40, lng: -70 })).toBe(0);
  });

  it("matches one degree of latitude", () => {
    const d = haversineMeters({ lat: 0, lng: 0 }, { lat: 1, lng: 0 });
    expect(d).toBeCloseTo(DEG_LAT_M, 0);
  });

  it("shrinks a degree of longitude by cos(lat)", () => {
    const atEquator = haversineMeters({ lat: 0, lng: 0 }, { lat: 0, lng: 1 });
    const at60 = haversineMeters({ lat: 60, lng: 0 }, { lat: 60, lng: 1 });
    // cos(60°) = 0.5, so the high-latitude degree is ~half as wide.
    expect(at60 / atEquator).toBeCloseTo(0.5, 2);
  });

  it("is symmetric", () => {
    const a = { lat: 41.1, lng: -71.5 };
    const b = { lat: 41.4, lng: -70.9 };
    expect(haversineMeters(a, b)).toBeCloseTo(haversineMeters(b, a), 6);
  });
});

describe("pathLengthMeters", () => {
  it("is zero for fewer than two points", () => {
    expect(pathLengthMeters([])).toBe(0);
    expect(pathLengthMeters([{ lat: 1, lng: 1 }])).toBe(0);
  });

  it("sums consecutive legs", () => {
    const pts: LatLng[] = [
      { lat: 0, lng: 0 },
      { lat: 1, lng: 0 },
      { lat: 2, lng: 0 },
    ];
    expect(pathLengthMeters(pts)).toBeCloseTo(2 * DEG_LAT_M, 0);
  });
});

describe("decimateByDistance", () => {
  it("returns short inputs unchanged", () => {
    const pts = [
      { lat: 0, lng: 0 },
      { lat: 1, lng: 1 },
    ];
    expect(decimateByDistance(pts, 100)).toEqual(pts);
  });

  it("always keeps first and last", () => {
    // ~10 m steps in latitude; decimate at 100 m.
    const pts: LatLng[] = [];
    for (let i = 0; i <= 100; i++) pts.push({ lat: 40 + i * 0.00009, lng: -70 });
    const out = decimateByDistance(pts, 100);
    expect(out[0]).toEqual(pts[0]);
    expect(out[out.length - 1]).toEqual(pts[pts.length - 1]);
  });

  it("spaces kept points by roughly the granularity", () => {
    const pts: LatLng[] = [];
    for (let i = 0; i <= 100; i++) pts.push({ lat: 40 + i * 0.00009, lng: -70 });
    // ~1000 m total / 100 m granularity → ~11 points (10 gaps + tail).
    expect(decimateByDistance(pts, 100).length).toBe(11);
  });

  it("collapses sub-granularity jitter", () => {
    const pts: LatLng[] = [];
    for (let i = 0; i < 100; i++) {
      pts.push({ lat: 40 + (i % 3) * 0.00001, lng: -70 + (i % 2) * 0.00001 });
    }
    pts.push({ lat: 40.01, lng: -70 }); // one far point
    // All jitter is within a few meters → first + far point only.
    expect(decimateByDistance(pts, 50).length).toBe(2);
  });
});

describe("douglasPeucker", () => {
  it("collapses a straight line to its endpoints", () => {
    const line: LatLng[] = [];
    for (let i = 0; i < 50; i++) line.push({ lat: 40 + i * 0.001, lng: -70 });
    expect(douglasPeucker(line, 5)).toHaveLength(2);
  });

  it("preserves the corners of a square", () => {
    const cornerLat = 0.009;
    const cornerLng = 0.009 / Math.cos((40 * Math.PI) / 180);
    const A = { lat: 40, lng: -70 };
    const B = { lat: 40, lng: -70 + cornerLng };
    const C = { lat: 40 + cornerLat, lng: -70 + cornerLng };
    const D = { lat: 40 + cornerLat, lng: -70 };
    const sq: LatLng[] = [];
    const edge = (a: LatLng, b: LatLng) => {
      for (let i = 0; i < 20; i++) {
        sq.push({
          lat: a.lat + ((b.lat - a.lat) * i) / 20,
          lng: a.lng + ((b.lng - a.lng) * i) / 20,
        });
      }
    };
    edge(A, B);
    edge(B, C);
    edge(C, D);
    edge(D, A);
    sq.push({ ...A });
    // 4 corners + the closing return to A.
    expect(douglasPeucker(sq, 10)).toHaveLength(5);
  });

  it("keeps more points as tolerance tightens", () => {
    // A bump in the middle of an otherwise straight line.
    const pts: LatLng[] = [
      { lat: 40, lng: -70 },
      { lat: 40.001, lng: -69.999 },
      { lat: 40.002, lng: -70 },
    ];
    expect(douglasPeucker(pts, 1000).length).toBe(2); // bump within tolerance → dropped
    expect(douglasPeucker(pts, 10).length).toBe(3); // bump exceeds tolerance → kept
  });
});

describe("simplifyTrack", () => {
  it("drops invalid coordinates and the time field", () => {
    const out = simplifyTrack(
      [
        { lat: NaN, lng: 0 },
        { lat: 0, lng: 200 },
        { lat: 40, lng: -70, t: 5 },
        { lat: 40.001, lng: -70 },
      ],
      { granularityMeters: 1 }
    );
    expect(out.inputCount).toBe(2);
    expect(out.waypoints).toHaveLength(2);
    expect(out.waypoints[0]).toEqual({ lat: 40, lng: -70 });
    expect("t" in out.waypoints[0]).toBe(false);
  });

  it("returns two-or-fewer point inputs untouched", () => {
    const out = simplifyTrack([{ lat: 1, lng: 1 }], { granularityMeters: 100 });
    expect(out.waypoints).toHaveLength(1);
    expect(out.capped).toBe(false);
  });

  it("reduces a dense straight track to its endpoints", () => {
    const pts: LatLng[] = [];
    for (let i = 0; i < 200; i++) pts.push({ lat: 40 + i * 0.0001, lng: -70 });
    const out = simplifyTrack(pts, { granularityMeters: 50 });
    expect(out.waypoints).toHaveLength(2);
    expect(out.capped).toBe(false);
  });

  it("caps output at maxPoints and flags it", () => {
    // A high-amplitude zigzag DP can't reduce below its corner count, so the
    // maxPoints escalation must kick in.
    const pts: LatLng[] = [];
    for (let i = 0; i < 100; i++) {
      pts.push({ lat: 40 + i * 0.001, lng: -70 + (i % 2) * 0.0005 });
    }
    const out = simplifyTrack(pts, { granularityMeters: 1, maxPoints: 10 });
    expect(out.capped).toBe(true);
    expect(out.waypoints.length).toBeLessThanOrEqual(10);
    expect(out.waypoints.length).toBeGreaterThanOrEqual(2);
  });

  it("does not cap when the route already fits", () => {
    const pts: LatLng[] = [];
    for (let i = 0; i < 50; i++) pts.push({ lat: 40 + i * 0.001, lng: -70 });
    const out = simplifyTrack(pts, { granularityMeters: 10, maxPoints: 500 });
    expect(out.capped).toBe(false);
  });
});
