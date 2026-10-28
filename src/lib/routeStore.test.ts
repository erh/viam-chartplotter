import { describe, it, expect } from "vitest";
import type { JsonValue, Route as ApiRoute, Waypoint } from "@viamrobotics/sdk";
import {
  listRoutes,
  pairRoutes,
  createRoute,
  replaceRouteWaypoints,
  updateRoute,
  deleteRoute,
  sizeWarning,
  nextColor,
  type Route,
  type RoutesApi,
} from "./routeStore";
import { latLngWaypoint } from "./navRoute";

// Records every call the wrapper issues and returns canned responses. The heavy
// read-modify-write / guard logic lives + is tested in Go (nav_routes_test.go);
// here we only verify the browser shapes the calls right and maps responses
// back.
class FakeApi implements RoutesApi {
  calls: [string, ...unknown[]][] = [];
  apiRoutes: ApiRoute[] = [];
  meta: JsonValue = {};
  async listRoutes(): Promise<ApiRoute[]> {
    this.calls.push(["listRoutes"]);
    return this.apiRoutes;
  }
  async addRoute(name: string, waypoints: Waypoint[], extra?: Record<string, JsonValue>) {
    this.calls.push(["addRoute", name, waypoints, extra]);
  }
  async updateRoute(
    name: string,
    newName?: string,
    waypoints?: Waypoint[],
    extra?: Record<string, JsonValue>
  ) {
    this.calls.push(["updateRoute", name, newName, waypoints, extra]);
  }
  async deleteRoute(name: string) {
    this.calls.push(["deleteRoute", name]);
  }
  async doCommand(cmd: Record<string, JsonValue>): Promise<JsonValue> {
    this.calls.push(["doCommand", cmd]);
    return this.meta;
  }
  last(): [string, ...unknown[]] {
    return this.calls[this.calls.length - 1];
  }
}

function apiRoute(name: string, ...pts: [number, number][]): ApiRoute {
  return { name, waypoints: pts.map(([lat, lng]) => latLngWaypoint({ lat, lng })) };
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

function meta(r: Route): Omit<Route, "waypoints"> {
  const { waypoints: _w, ...rest } = r;
  return rest;
}

describe("listRoutes", () => {
  it("joins ListRoutes waypoints with routes_meta", async () => {
    const api = new FakeApi();
    api.apiRoutes = [apiRoute("Test", [41.1, -71.5], [41.2, -71.4])];
    api.meta = { routes: [meta(mkRoute({ id: "x", scope: "parent" }))] };
    const routes = await listRoutes(api);
    expect(api.calls).toContainEqual(["doCommand", { routes_meta: true }]);
    expect(routes).toEqual([mkRoute({ id: "x", scope: "parent" })]);
  });

  it("defaults to empty when the meta routes field is missing", async () => {
    const api = new FakeApi();
    api.apiRoutes = [apiRoute("Test", [1, 1], [2, 2])];
    expect(await listRoutes(api)).toEqual([]);
  });
});

describe("pairRoutes", () => {
  it("pairs by index, keeping duplicate names apart", () => {
    const routes = pairRoutes(
      [apiRoute("Dup", [1, 1], [2, 2]), apiRoute("Dup", [3, 3], [4, 4])],
      [meta(mkRoute({ id: "a", name: "Dup" })), meta(mkRoute({ id: "b", name: "Dup" }))]
    );
    expect(routes.map((r) => [r.id, r.waypoints[0].lat])).toEqual([
      ["a", 1],
      ["b", 3],
    ]);
  });

  it("falls back to name when the lists don't line up, dropping unmatched routes", () => {
    const routes = pairRoutes(
      [apiRoute("New", [9, 9], [8, 8]), apiRoute("Old", [1, 1], [2, 2])],
      [meta(mkRoute({ id: "o", name: "Old" }))]
    );
    expect(routes).toHaveLength(1);
    expect(routes[0].id).toBe("o");
    expect(routes[0].waypoints[0]).toEqual({ lat: 1, lng: 1 });
  });
});

describe("createRoute", () => {
  it("calls AddRoute with library fields in extra", async () => {
    const api = new FakeApi();
    await createRoute(api, {
      name: "N",
      source: "track",
      color: "#123456",
      waypoints: [
        { lat: 1, lng: 2 },
        { lat: 3, lng: 4 },
      ],
    });
    expect(api.last()).toEqual([
      "addRoute",
      "N",
      [latLngWaypoint({ lat: 1, lng: 2 }), latLngWaypoint({ lat: 3, lng: 4 })],
      { source: "track", color: "#123456" },
    ]);
  });
});

describe("replaceRouteWaypoints", () => {
  it("calls UpdateRoute by id with only waypoints", async () => {
    const api = new FakeApi();
    await replaceRouteWaypoints(api, "rte_a", [{ lat: 1, lng: 2 }]);
    expect(api.last()).toEqual([
      "updateRoute",
      "rte_a",
      undefined,
      [latLngWaypoint({ lat: 1, lng: 2 })],
      undefined,
    ]);
  });
});

describe("updateRoute", () => {
  it("sends the new name and puts color/notes in extra", async () => {
    const api = new FakeApi();
    await updateRoute(api, "rte_a", { name: "New", color: "#123456" });
    expect(api.last()).toEqual(["updateRoute", "rte_a", "New", undefined, { color: "#123456" }]);
  });
});

describe("deleteRoute", () => {
  it("calls DeleteRoute with the id", async () => {
    const api = new FakeApi();
    await deleteRoute(api, "rte_a");
    expect(api.last()).toEqual(["deleteRoute", "rte_a"]);
  });
});
