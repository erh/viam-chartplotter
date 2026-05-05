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

| Name              | Type   | Required | Description                                                                                                                                                  |
| ----------------- | ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `movement_sensor` | string | no       | Name of a movement sensor on the same machine. When set, the service's `Location` method reports that sensor's live position and compass heading.            |
| `data_path`       | string | no       | Absolute path to the JSON file used to persist waypoints. Defaults to `<user-cache-dir>/viam-chartplotter/nav/<service-name>.json`.                          |

### Sample config

```json
{
  "name": "nav",
  "namespace": "rdk",
  "type": "navigation",
  "model": "erh:viam-chartplotter:nav",
  "attributes": {
    "movement_sensor": "gps",
    "data_path": "/var/lib/viam-chartplotter/nav.json"
  },
  "depends_on": ["gps"]
}
```

Both `attributes` fields are optional. The minimal config is just `model`
plus `name`/`type`/`namespace`; in that case `Location` returns (0, 0) and
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
