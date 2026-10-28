import { describe, it, expect } from "vitest";
import {
  GoalState,
  NavMode,
  type GoalStatus,
  type Position,
  type Waypoint,
} from "@viamrobotics/sdk";
import {
  activeRouteFromGoal,
  latLngWaypoint,
  routeUpdate,
  withAppended,
  withInserted,
  withMoved,
  withRemoved,
} from "./navRoute";

function pos(lat: number, lng: number): Position {
  return {
    value: {
      case: "geoPose",
      value: {
        location: { latitude: lat, longitude: lng },
        orientation: { case: "heading", value: 0 },
      },
    },
  };
}

function wp(lat: number, lng: number): Waypoint {
  return { value: { case: "position", value: pos(lat, lng) } };
}

function goal(g: Pick<GoalStatus, "state"> & Partial<GoalStatus>): GoalStatus {
  return { mode: NavMode.UNSPECIFIED, ...g };
}

function routeGoal(state: GoalState, index: number, ...pts: [number, number][]): GoalStatus {
  return goal({
    state,
    mode: NavMode.ROUTE,
    target: pos(...pts[Math.min(index, pts.length - 1)]),
    route: { name: "Run", index, loop: false, lap: 0, waypoints: pts.map((p) => wp(...p)) },
  });
}

describe("activeRouteFromGoal", () => {
  it("splits a route at its cursor, ids are absolute indices", () => {
    const r = activeRouteFromGoal(routeGoal(GoalState.ACTIVE, 1, [1, 1], [2, 2], [3, 3]));
    expect(r).toEqual({
      reached: [{ lat: 1, lng: 1 }],
      remaining: [
        { id: "1", lat: 2, lng: 2 },
        { id: "2", lat: 3, lng: 3 },
      ],
      suspended: false,
      routeName: "Run",
    });
  });

  it("keeps a suspended route", () => {
    const r = activeRouteFromGoal(routeGoal(GoalState.SUSPENDED, 0, [1, 1]));
    expect(r?.suspended).toBe(true);
    expect(r?.remaining).toHaveLength(1);
  });

  it("has no route for a missing or finished goal", () => {
    expect(activeRouteFromGoal(undefined)).toBeNull();
    for (const s of [
      GoalState.SUCCEEDED,
      GoalState.CANCELED,
      GoalState.FAILED,
      GoalState.UNSPECIFIED,
    ]) {
      expect(activeRouteFromGoal(routeGoal(s, 0, [1, 1]))).toBeNull();
    }
  });

  it("treats a target-only goal as one waypoint", () => {
    const r = activeRouteFromGoal(goal({ state: GoalState.ACTIVE, target: pos(5, 6) }));
    expect(r?.reached).toEqual([]);
    expect(r?.remaining).toEqual([{ id: "0", lat: 5, lng: 6 }]);
  });

  it("skips location-name waypoints", () => {
    const g = routeGoal(GoalState.ACTIVE, 0, [1, 1]);
    g.route!.waypoints.unshift({ value: { case: "location", value: "dock" } });
    g.route!.index = 0;
    expect(activeRouteFromGoal(g)?.remaining).toEqual([{ id: "1", lat: 1, lng: 1 }]);
  });
});

describe("edits", () => {
  const remaining = [
    { id: "1", lat: 2, lng: 2 },
    { id: "2", lat: 3, lng: 3 },
  ];

  it("appends", () => {
    expect(withAppended(remaining, { lat: 9, lng: 9 }).at(-1)).toEqual({ lat: 9, lng: 9 });
  });

  it("inserts before an id, or appends when unknown", () => {
    expect(withInserted(remaining, "2", { lat: 9, lng: 9 }).map((p) => p.lat)).toEqual([2, 9, 3]);
    expect(withInserted(remaining, "", { lat: 9, lng: 9 }).map((p) => p.lat)).toEqual([2, 3, 9]);
    expect(withInserted(remaining, "nope", { lat: 9, lng: 9 }).map((p) => p.lat)).toEqual([
      2, 3, 9,
    ]);
  });

  it("moves and removes by id", () => {
    expect(withMoved(remaining, "1", { lat: 7, lng: 7 }).map((p) => p.lat)).toEqual([7, 3]);
    expect(withRemoved(remaining, "1").map((p) => p.lat)).toEqual([3]);
  });
});

describe("routeUpdate", () => {
  it("re-sends the reached prefix and starts at the same cursor", () => {
    const active = activeRouteFromGoal(routeGoal(GoalState.ACTIVE, 1, [1, 1], [2, 2]))!;
    expect(routeUpdate(active, withAppended(active.remaining, { lat: 4, lng: 4 }))).toEqual({
      kind: "navigate",
      waypoints: [
        { lat: 1, lng: 1 },
        { lat: 2, lng: 2 },
        { lat: 4, lng: 4 },
      ],
      startIndex: 1,
    });
  });

  it("starts fresh with no open goal", () => {
    expect(routeUpdate(null, [{ lat: 1, lng: 1 }])).toEqual({
      kind: "navigate",
      waypoints: [{ lat: 1, lng: 1 }],
      startIndex: 0,
    });
  });

  it("cancels when nothing remains", () => {
    const active = activeRouteFromGoal(routeGoal(GoalState.ACTIVE, 1, [1, 1], [2, 2]))!;
    expect(routeUpdate(active, withRemoved(active.remaining, "1"))).toEqual({ kind: "cancel" });
  });
});

describe("latLngWaypoint", () => {
  it("is a geoPose waypoint with a zero heading", () => {
    expect(latLngWaypoint({ lat: 1.5, lng: -2 })).toEqual(wp(1.5, -2));
  });

  it("round-trips through activeRouteFromGoal", () => {
    const g = goal({
      state: GoalState.ACTIVE,
      route: {
        name: "",
        index: 0,
        loop: false,
        lap: 0,
        waypoints: [latLngWaypoint({ lat: 3, lng: 4 })],
      },
    });
    expect(activeRouteFromGoal(g)?.remaining).toEqual([{ id: "0", lat: 3, lng: 4 }]);
  });
});
