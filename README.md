## Viam Chartplotter

## ToDo

- display for what data old and restart
- maps
  ** option for ais tracks and prediction
  ** COG / Heading for boat + ais
- Camera - make streaming when fixed
- History
- Configuration
- Multiple tabs
- Later Cool Stuff
  \*\* tides

## Navigation service

The `erh:viam-chartplotter:nav` model is an `rdk:service:navigation`
implementation backed by an in-memory waypoint list that is mirrored to a JSON
file on disk so waypoints survive module restarts. The chartplotter UI auto-
detects this service, draws the current waypoints as an amber dashed route
from the boat through each waypoint, and exposes buttons to add a waypoint at
the boat's current position, drop one by clicking on the chart, or clear the
whole route.

### Configuration attributes

| Name                | Type   | Required | Description                                                                                                                                                                                                                                  |
| ------------------- | ------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `movement_sensor`   | string | no       | Name of a movement sensor on the same machine. When set, the service's `Location` method reports that sensor's live position and compass heading, and the auto-arrival poller uses it to detect waypoint arrivals.                           |
| `data_path`         | string | no       | Absolute path to the JSON file used to persist waypoints. Defaults to `<user-cache-dir>/viam-chartplotter/nav/<service-name>.json`.                                                                                                          |
| `arrival_radius_m`  | number | no       | When `movement_sensor` is set, the next waypoint is automatically marked visited (and disappears from the route) once the boat is within this many meters of it. Defaults to `200`. Set to a negative number to disable, or omit to use the default. |

### Sample config

```json
{
  "name": "nav",
  "namespace": "rdk",
  "type": "navigation",
  "model": "erh:viam-chartplotter:nav",
  "attributes": {
    "movement_sensor": "gps",
    "data_path": "/var/lib/viam-chartplotter/nav.json",
    "arrival_radius_m": 200
  }
}
```

Both `attributes` fields are optional — the dependency on `movement_sensor`
is reported automatically from the service's config validator, so no
explicit `depends_on` is needed. The minimal config is just `model` plus
`name`/`type`/`namespace`; in that case `Location` returns (0, 0) and
waypoints are written to the default cache path.

## Developing

```
npm install
npm run dev
```

## Building

To create a production version of your app:

```bash
npm run build
```

You can preview the production build with `npm run preview`.

> To deploy your app, you may need to install an [adapter](https://kit.svelte.dev/docs/adapters) for your target environment.
