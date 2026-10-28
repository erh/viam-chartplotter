// Pure model of the nav service's active route, shared shape with the mobile
// app (mobile/lib/routes/nav_route.dart ports this file and its tests).
//
// The navigation API has no "waypoint list" — the active route is the open
// goal reported by GetNavigationStatus: goal.route.waypoints with a cursor
// (goal.route.index), the waypoints before it already reached. Clients edit
// the route by re-sending the whole list with NavigateRoute, keeping the
// reached prefix and starting at the same cursor; an empty route is a Cancel.

import { GoalState, type GoalStatus, type Position, type Waypoint } from "@viamrobotics/sdk";

export interface LatLng {
  lat: number;
  lng: number;
}

/** A remaining waypoint. id is its index in the goal's full list, as a string. */
export interface NavWaypoint extends LatLng {
  id: string;
}

export interface ActiveRoute {
  /** Waypoints already reached (kept on resend so the trip log survives). */
  reached: LatLng[];
  /** Waypoints still to go, the first being the current target. */
  remaining: NavWaypoint[];
  suspended: boolean;
  /** Saved route name, when the goal was started from one. */
  routeName: string;
}

/** What to send to apply an edit: a new route, or a cancel when it's empty. */
export type RouteUpdate =
  | { kind: "navigate"; waypoints: LatLng[]; startIndex: number }
  | { kind: "cancel" };

function positionLatLng(p: Position | undefined): LatLng | null {
  if (p?.value.case !== "geoPose") return null;
  const loc = p.value.value.location;
  if (!loc) return null;
  return { lat: loc.latitude, lng: loc.longitude };
}

/** A waypoint's lat/lng, or null for a named location or a local-frame pose. */
export function waypointLatLng(w: Waypoint): LatLng | null {
  return w.value.case === "position" ? positionLatLng(w.value.value) : null;
}

/**
 * A lat/lng as a NavigateRoute waypoint. The server requires a heading (or
 * orientation) on every GeoPose; a waypoint has none, so it sends 0.
 */
export function latLngWaypoint(p: LatLng): Waypoint {
  return {
    value: {
      case: "position",
      value: {
        value: {
          case: "geoPose",
          value: {
            location: { latitude: p.lat, longitude: p.lng },
            orientation: { case: "heading", value: 0 },
          },
        },
      },
    },
  };
}

/**
 * The active route in a goal, or null when there is no open (active or
 * suspended) goal. A goal without route progress (NavigateToPoint, GoTo...) is
 * a one-waypoint route to its target.
 */
export function activeRouteFromGoal(goal: GoalStatus | undefined): ActiveRoute | null {
  if (!goal || (goal.state !== GoalState.ACTIVE && goal.state !== GoalState.SUSPENDED)) {
    return null;
  }
  const suspended = goal.state === GoalState.SUSPENDED;
  if (goal.route) {
    const index = goal.route.index;
    const reached: LatLng[] = [];
    const remaining: NavWaypoint[] = [];
    goal.route.waypoints.forEach((w, i) => {
      // A waypoint that names a saved location carries no coordinates; this
      // service always inlines them, so skipping is only defensive.
      const p = waypointLatLng(w);
      if (!p) return;
      if (i < index) reached.push(p);
      else remaining.push({ id: String(i), ...p });
    });
    return { reached, remaining, suspended, routeName: goal.route.name };
  }
  const target = positionLatLng(goal.target);
  return {
    reached: [],
    remaining: target ? [{ id: "0", ...target }] : [],
    suspended,
    routeName: "",
  };
}

/** The NavigateRoute (or Cancel) that makes `remaining` the route to go. */
export function routeUpdate(active: ActiveRoute | null, remaining: LatLng[]): RouteUpdate {
  if (remaining.length === 0) return { kind: "cancel" };
  const reached = active?.reached ?? [];
  return {
    kind: "navigate",
    waypoints: [...reached, ...remaining].map(({ lat, lng }) => ({ lat, lng })),
    startIndex: reached.length,
  };
}

export function withAppended(remaining: LatLng[], p: LatLng): LatLng[] {
  return [...remaining, p];
}

/** Insert p before the waypoint with beforeId; append when it isn't found. */
export function withInserted(remaining: NavWaypoint[], beforeId: string, p: LatLng): LatLng[] {
  const idx = beforeId ? remaining.findIndex((w) => w.id === beforeId) : -1;
  if (idx < 0) return [...remaining, p];
  return [...remaining.slice(0, idx), p, ...remaining.slice(idx)];
}

export function withMoved(remaining: NavWaypoint[], id: string, p: LatLng): LatLng[] {
  return remaining.map((w) => (w.id === id ? { ...w, ...p } : w));
}

export function withRemoved(remaining: NavWaypoint[], id: string): LatLng[] {
  return remaining.filter((w) => w.id !== id);
}
