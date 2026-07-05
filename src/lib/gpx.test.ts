import { describe, it, expect } from "vitest";
import { routeToGpx, gpxFilename } from "./gpx";

describe("routeToGpx", () => {
  it("emits a route with named points and 1e-7 precision", () => {
    const xml = routeToGpx("Block Island Run", [
      { lat: 41.1631, lng: -71.5784 },
      { lat: 41.2042, lng: -71.5511 },
    ]);
    expect(xml).toContain(`<gpx version="1.1"`);
    expect(xml).toContain("<name>Block Island Run</name>");
    expect(xml).toContain(`<rtept lat="41.1631000" lon="-71.5784000">`);
    expect(xml).toContain("<name>Block Island Run 01</name>");
    expect(xml).toContain("<name>Block Island Run 02</name>");
    expect((xml.match(/<rtept /g) ?? []).length).toBe(2);
  });

  it("escapes XML in names", () => {
    const xml = routeToGpx(`Tom & Jerry's <run>`, [{ lat: 1, lng: 2 }]);
    expect(xml).toContain("<name>Tom &amp; Jerry&apos;s &lt;run&gt;</name>");
    expect(xml).not.toContain("<run>");
  });

  it("falls back to a default name", () => {
    const xml = routeToGpx("  ", [{ lat: 1, lng: 2 }]);
    expect(xml).toContain("<name>Route</name>");
    expect(xml).toContain("<name>Route 01</name>");
  });
});

describe("gpxFilename", () => {
  it("slugifies", () => {
    expect(gpxFilename("Block Island Run")).toBe("block-island-run.gpx");
    expect(gpxFilename("  A / B?  ")).toBe("a-b.gpx");
    expect(gpxFilename("!!!")).toBe("route.gpx");
  });
});
