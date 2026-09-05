import { describe, it, expect } from "vitest";
import { autoRouteUrl, routeCautions, metresToNm, type AutoRouteResult } from "./autoRoute";

const start = { lat: 41.0, lng: -71.5 };
const end = { lat: 41.1, lng: -71.4 };

describe("autoRouteUrl", () => {
  it("sends the endpoints and nothing else by default", () => {
    const url = new URL(autoRouteUrl("https://charts.example", { start, end }));
    expect(url.pathname).toBe("/noaa-enc/autoroute");
    expect(url.searchParams.get("startLat")).toBe("41");
    expect(url.searchParams.get("startLon")).toBe("-71.5");
    expect(url.searchParams.get("endLat")).toBe("41.1");
    expect(url.searchParams.get("endLon")).toBe("-71.4");
    // Omitted, so the server falls back to the boat's configured draft.
    expect(url.searchParams.has("sd")).toBe(false);
    expect(url.searchParams.has("ideal")).toBe(false);
  });

  it("passes depths in feet and the avoid list", () => {
    const url = new URL(
      autoRouteUrl("", { start, end, safeDepthFt: 7, idealDepthFt: 20, avoid: ["restricted"] }),
      "http://x"
    );
    expect(url.searchParams.get("sd")).toBe("7");
    expect(url.searchParams.get("ideal")).toBe("20");
    expect(url.searchParams.get("avoid")).toBe("restricted");
  });

  it("drops non-positive depths rather than sending a zero", () => {
    const url = new URL(
      autoRouteUrl("", { start, end, safeDepthFt: 0, idealDepthFt: -1 }),
      "http://x"
    );
    expect(url.searchParams.has("sd")).toBe(false);
    expect(url.searchParams.has("ideal")).toBe(false);
  });

  it("keeps a zero clearance, which is a real choice", () => {
    const url = new URL(autoRouteUrl("", { start, end, clearanceM: 0 }), "http://x");
    expect(url.searchParams.get("clearance")).toBe("0");
  });

  it("uses a same-origin path when there is no separate chart server", () => {
    expect(autoRouteUrl("", { start, end })).toMatch(/^\/noaa-enc\/autoroute\?/);
  });
});

function result(over: Partial<AutoRouteResult> = {}): AutoRouteResult {
  return {
    waypoints: [start, end],
    distance_meters: 10000,
    direct_meters: 9000,
    min_depth_meters: 3.048, // 10 ft
    crossed_unknown: false,
    safe_depth_meters: 1.8,
    ideal_depth_meters: 3.6,
    snapped_start: false,
    snapped_end: false,
    cell_size_meters: 25,
    sections: 1,
    ...over,
  };
}

describe("routeCautions", () => {
  it("reports the shoalest charted depth in feet", () => {
    expect(routeCautions(result())).toContain("shoalest charted depth on the route: 10.0 ft");
  });

  it("flags uncharted water", () => {
    const out = routeCautions(result({ crossed_unknown: true }));
    expect(out.some((c) => c.includes("no charted depth"))).toBe(true);
  });

  it("passes the server's own warnings through", () => {
    const out = routeCautions(result({ warnings: ["start moved to the nearest navigable water"] }));
    expect(out[0]).toBe("start moved to the nearest navigable water");
  });

  it("omits the depth line when nothing was charted", () => {
    const out = routeCautions(result({ min_depth_meters: null, crossed_unknown: true }));
    expect(out.some((c) => c.startsWith("shoalest"))).toBe(false);
  });
});

describe("routeCautions section warning", () => {
  it("says nothing about sections for a route planned in one piece", () => {
    expect(routeCautions(result()).some((c) => c.includes("sections"))).toBe(false);
  });

  it("reports a sectioned route and the coarsest grid it used", () => {
    const out = routeCautions(result({ sections: 4, cell_size_meters: 116 }));
    expect(out.some((c) => c.includes("split into 4 sections") && c.includes("116 m"))).toBe(true);
  });
});

describe("metresToNm", () => {
  it("converts using the international nautical mile", () => {
    expect(metresToNm(1852)).toBeCloseTo(1, 9);
  });
});
