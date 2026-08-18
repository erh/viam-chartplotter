// GPX 1.1 export of a waypoint list, shaped for chartplotter import (Garmin
// units read routes from Garmin/GPX/*.gpx on a memory card). The route is
// emitted as one <rte> whose <rtept>s carry sequential names so they're easy
// to identify after import.
import type { LatLng } from "./simplify";

function escapeXml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&apos;");
}

// Waypoint names inside the route: "<prefix> 01", "<prefix> 02", … where the
// prefix is the route name truncated to keep names short on plotter screens.
function pointName(routeName: string, i: number): string {
  const prefix = routeName.trim().slice(0, 20) || "WP";
  return `${prefix} ${String(i + 1).padStart(2, "0")}`;
}

export function routeToGpx(name: string, waypoints: LatLng[]): string {
  const displayName = name.trim() || "Route";
  const pts = waypoints
    .map(
      (w, i) =>
        `    <rtept lat="${w.lat.toFixed(7)}" lon="${w.lng.toFixed(7)}">\n` +
        `      <name>${escapeXml(pointName(displayName, i))}</name>\n` +
        `    </rtept>`
    )
    .join("\n");
  return (
    `<?xml version="1.0" encoding="UTF-8"?>\n` +
    `<gpx version="1.1" creator="viam-chartplotter" xmlns="http://www.topografix.com/GPX/1/1">\n` +
    `  <rte>\n` +
    `    <name>${escapeXml(displayName)}</name>\n` +
    pts +
    `\n  </rte>\n` +
    `</gpx>\n`
  );
}

// Filesystem-safe download name derived from the route name.
export function gpxFilename(name: string): string {
  const slug = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return `${slug || "route"}.gpx`;
}

export function downloadGpx(name: string, waypoints: LatLng[]): void {
  const blob = new Blob([routeToGpx(name, waypoints)], { type: "application/gpx+xml" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = gpxFilename(name);
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
