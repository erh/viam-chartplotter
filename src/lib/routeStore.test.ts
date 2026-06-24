import { describe, it, expect } from "vitest";
import {
  listRoutes,
  saveRoute,
  deleteRoute,
  renameRoute,
  sizeWarning,
  newRouteId,
  nextColor,
  type Route,
  type RoutesApi,
} from "./routeStore";

// Records every DoCommand the wrapper issues and returns a canned response. The
// heavy read-modify-write / guard logic now lives + is tested in Go
// (nav_routes_test.go); here we only verify the browser shapes the commands
// right and maps responses back.
class FakeApi implements RoutesApi {
  calls: Record<string, unknown>[] = [];
  response: Record<string, unknown> = {};
  async doCommand(cmd: Record<string, unknown>): Promise<Record<string, unknown>> {
    this.calls.push(cmd);
    return this.response;
  }
  last(): Record<string, unknown> {
    return this.calls[this.calls.length - 1];
  }
}

function mkRoute(overrides: Partial<Route> = {}): Route {
  const now = "2026-06-19T00:00:00Z";
  return {
    id: "rte_a",
    name: "Test",
    source: "manual",
    color: "#ff8800",
    createdAt: now,
    updatedAt: now,
    waypoints: [
      { lat: 41.1, lng: -71.5 },
      { lat: 41.2, lng: -71.4 },
    ],
    ...overrides,
  };
}

describe("newRouteId", () => {
  it("has the rte_ prefix and is unique across calls", () => {
    const a = newRouteId();
    const b = newRouteId();
    expect(a).toMatch(/^rte_[a-z0-9]+_[0-9a-f]{4}$/);
    expect(a).not.toBe(b);
  });
});

describe("nextColor", () => {
  it("returns an unused palette color", () => {
    expect(nextColor([mkRoute({ color: "#ff8800" })])).not.toBe("#ff8800");
  });
  it("falls back to a cycled color when the palette is exhausted", () => {
    const many = Array.from({ length: 20 }, (_, i) => mkRoute({ id: `r${i}`, color: `#${i}` }));
    expect(typeof nextColor(many)).toBe("string");
  });
});

describe("sizeWarning", () => {
  it("is false for a small set and true for a large one", () => {
    expect(sizeWarning([mkRoute()])).toBe(false);
    const big = Array.from({ length: 50 }, (_, i) =>
      mkRoute({
        id: `r${i}`,
        waypoints: Array.from({ length: 2000 }, () => ({ lat: 41.123456, lng: -71.123456 })),
      })
    );
    expect(sizeWarning(big)).toBe(true);
  });
});

describe("listRoutes", () => {
  it("issues routes_list and returns the routes", async () => {
    const api = new FakeApi();
    api.response = { routes: [mkRoute({ id: "x" })] };
    const routes = await listRoutes(api);
    expect(api.last()).toEqual({ routes_list: true });
    expect(routes).toHaveLength(1);
    expect(routes[0].id).toBe("x");
  });

  it("defaults to empty when the routes field is missing", async () => {
    const api = new FakeApi();
    api.response = {};
    expect(await listRoutes(api)).toEqual([]);
  });
});

describe("saveRoute", () => {
  it("issues routes_save with the route", async () => {
    const api = new FakeApi();
    api.response = { ok: true, scope: "location" };
    const r = mkRoute();
    await saveRoute(api, r);
    expect(api.last()).toEqual({ routes_save: { route: r } });
  });
});

describe("deleteRoute", () => {
  it("issues routes_delete with the id", async () => {
    const api = new FakeApi();
    await deleteRoute(api, "rte_a");
    expect(api.last()).toEqual({ routes_delete: { id: "rte_a" } });
  });
});

describe("renameRoute", () => {
  it("issues routes_rename with fields + updatedAt", async () => {
    const api = new FakeApi();
    await renameRoute(api, "rte_a", { name: "New", color: "#123456" }, "2026-07-01T00:00:00Z");
    expect(api.last()).toEqual({
      routes_rename: {
        id: "rte_a",
        name: "New",
        color: "#123456",
        updatedAt: "2026-07-01T00:00:00Z",
      },
    });
  });
});
