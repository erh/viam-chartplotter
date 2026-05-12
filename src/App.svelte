<script lang="ts">
  import { getCookie, setCookie } from "typescript-cookie";
  // import '@viamrobotics/prime-core/prime.css';
  import { onMount, onDestroy } from "svelte";
  import { Icon as PrimeIcon } from "@viamrobotics/prime-core";

  import { Logger } from "tslog";
  import type { BoatInfo } from "./lib/BoatInfo";

  import { Coordinate } from "tsgeo/Coordinate";
  import { DecimalMinutes } from "tsgeo/Formatter/Coordinate/DecimalMinutes";

  import { BSON } from "bsonfy";

  import { LinkedChart } from "svelte-tiny-linked-charts";

  import * as VIAM from "@viamrobotics/sdk";

  import { tankSort } from "./helpers.ts";
  import MarineMap from "./marineMap.svelte";
  import YachtDetails from "./YachtDetails.svelte";

  const globalLogger = new Logger({ name: "global" });
  let globalClient: VIAM.RobotClient;
  let globalClientLastReset = new Date();
  let globalClientCloudMetaData = null;

  let globalCloudClient: VIAM.ViamClient;

  // Cache of cloud clients for remote parts, keyed by the remote's name
  // (the prefix segment that components from that remote carry, e.g.
  // "myremote" in "myremote:seatemp"). Lazily created on first query
  // for a remote-borne component. Cached for the lifetime of the page
  // so we don't spin a new client on every poll.
  let remoteCloudClients: Record<string, Promise<VIAM.ViamClient>> = {};

  // Track timeout IDs and blob URLs for cleanup
  let updateLoopTimeout: number | undefined;
  let cloudLoopTimeout: number | undefined;
  let versionLoopTimeout: number | undefined;
  let cameraBlobUrls: Record<string, string> = {};

  // Server instance ID seen on first successful /version fetch. When the
  // module restarts (e.g. after an upgrade) the ID changes and we reload
  // so the browser picks up the new build without a manual refresh.
  let serverInstanceID: string | null = null;

  async function checkServerVersion() {
    try {
      const resp = await fetch("/version", { cache: "no-store" });
      if (resp.ok) {
        const data = await resp.json();
        const id = data?.instance;
        if (typeof id === "string" && id !== "") {
          if (serverInstanceID === null) {
            serverInstanceID = id;
          } else if (id !== serverInstanceID) {
            window.location.reload();
            return;
          }
        }
      }
    } catch {
      // Server unreachable (mid-restart). Try again on the next tick.
    }
    versionLoopTimeout = setTimeout(checkServerVersion, 10000);
  }

  let globalData = $state({
    pos: new Coordinate(0, 0),
    posHistory: [],
    posHistoryLastCheck: 0,
    posString: "n/a",
    speed: 0.0,
    temp: 0.0,
    depth: 0.0,
    showDepthOnTrack: false,
    heading: 0.0,
    cog: null as number | null,
    windSpeed: 0.0,
    windAngle: 0.0,
    spotZeroFW: 0.0,
    spotZeroSW: 0.0,
    seakeeperData: {
      power_available: 0,
      power_enabled: 0,
      stabilize_available: false,
      stabilize_enabled: false,
      progress_bar_percentage: 0.0,
    },
    gauges: {},
    acPowers: {},
    vicPowers: {},
    vicDoors: {},
    acPowerData: false,
    gaugesToHistorical: {},
    // Water-temperature history (15-min buckets, last 24h). Same shape
    // as gaugesToHistorical entries: { ts: <fetched-at>, data: [{_id,
    // ts, temp}] }. Filled by updateSeaTempHistory; rendered as a
    // sparkline next to the live Water Temp readout when populated.
    seaTempHistorical: null as { ts: Date; data: any[] } | null,

    allResources: [],
    machineStatus: {
      resources: [],
    },

    cameraNames: [],
    lastCameraTimes: [],

    navWaypoints: [] as { id: string; lat: number; lng: number }[],

    numUpdates: 0,
    status: "Not connected yet",
    statusLastError: new Date(),
    lastData: new Date(),

    partConfig: {},
    aisBoats: [] as BoatInfo[],
    enlargedImage: null,
    shortGraphRange:
      typeof window !== "undefined" &&
      new URLSearchParams(window.location.search).get("shortGraph") === "1",
    hideDataPanel: getCookie("hideDataPanel") === "1",
  });

  // When the depth-colour-track toggle flips (now controlled from the
  // map's layers panel), force a refetch of the position history so
  // the depth field is filled in (or cleared) on every track point.
  // Fires once on mount too — harmless, the next poll just refetches.
  $effect(() => {
    void globalData.showDepthOnTrack;
    globalData.posHistoryLastCheck = 0;
  });

  const SETTINGS_COOKIE_OPTS = { expires: 365, sameSite: "lax" as const, path: "/" };

  function toggleHideDataPanel() {
    globalData.hideDataPanel = !globalData.hideDataPanel;
    setCookie("hideDataPanel", globalData.hideDataPanel ? "1" : "0", SETTINGS_COOKIE_OPTS);
  }

  var globalConfig = $state({
    movementSensorName: "",
    movementSensorProps: {},
    movementSensorAlternates: [],
    movementSensorForQuery: "",

    aisSensorName: "",
    airstreamName: "",
    navServiceName: "",
    routeSensorName: "",
    seatempSensorName: "",
    depthSensorName: "",
    windSensorName: "",
    spotZeroFWSensorName: "",
    spotZeroSWSensorName: "",
    seakeeperSensorName: "",
    acPowers: [],
    vicPowerNames: [],
    vicDoorsSensorName: "",

    zoomModifier: 0,
  });

  // Airstream (viamboat aisstream) state. The map's "airstream" layer toggle
  // controls whether we fetch from the global aisstream feed and feed bounding
  // boxes to it. Until the bounding box is set we don't do anything — that's
  // the contract on the airstream component itself, mirrored on the client.
  let airstreamLayerActive = $state(false);
  let airstreamBboxDebounce: number | undefined;

  function gotNewData() {
    globalData.lastData = new Date();
  }

  function errorHandlerMaker(m) {
    return function (e) {
      return errorHandler(e, m);
    };
  }

  function errorHandler(e, context) {
    globalData.statusLastError = new Date();
    if (context) {
      globalLogger.error(context + " : " + e);
    } else {
      globalLogger.error(e);
    }
    var s = e.toString();
    globalData.status = "Error: " + s;
    if (context) {
      globalData.status = context + " : " + globalData.status;
    }

    var reset = false;

    var diff = new Date() - globalData.lastData;

    if (diff > 1000 * 30) {
      reset = true;
    }

    if (s.indexOf("SESSION_EXPIRED") >= 0) {
      reset = true;
    }

    if (reset && new Date() - globalClientLastReset > 1000 * 30) {
      globalLogger.warn("Forcing reconnect b/c session_expired");
      globalData.status = "Forcing reconnect b/c of error: " + e.toString();
      globalClient = null;
      globalClientLastReset = new Date();
    }
  }

  function doUpdate(loopNumber: number, client: VIAM.RobotClient) {
    const msClient = new VIAM.MovementSensorClient(client, globalConfig.movementSensorName);

    msClient
      .getPosition()
      .then((p) => {
        gotNewData();

        var myPos = new Coordinate(p.coordinate.latitude, p.coordinate.longitude);
        globalData.pos = myPos;

        // eslint-disable-next-line no-constant-condition
        if (false) {
          // this is being stupid on mobile
          var gpsFormatter = new DecimalMinutes();
          gpsFormatter.setSeparator("\n").useCardinalLetters(true);

          globalData.posString = gpsFormatter.format(myPos);
        } else {
          globalData.posString =
            p.coordinate.latitude.toFixed(5) + ", " + p.coordinate.longitude.toFixed(5);
        }
      })
      .catch(errorHandlerMaker("movement sensor"));

    msClient
      .getLinearVelocity()
      .then((v) => {
        globalData.speed = v.y * 1.94384;
      })
      .catch(errorHandlerMaker("linear velocity"));

    msClient
      .getCompassHeading()
      .then((ch) => {
        globalData.heading = ch;
      })
      .catch(errorHandlerMaker("compass"));

    msClient
      .getReadings()
      .then((r) => {
        var cog =
          r["Course Over Ground"] ??
          r["course_over_ground"] ??
          r["CourseOverGround"] ??
          r["cog"] ??
          r["COG"];
        if (typeof cog === "number" && !isNaN(cog)) {
          globalData.cog = cog;
        }
      })
      .catch(errorHandlerMaker("cog"));

    if (globalConfig.seatempSensorName != "") {
      new VIAM.SensorClient(client, globalConfig.seatempSensorName)
        .getReadings()
        .then((t) => {
          if (!isNaN(t.Temperature)) {
            globalData.temp = 32 + t.Temperature * 1.8;
          }
        })
        .catch(errorHandlerMaker("seatemp"));
    }

    if (globalConfig.depthSensorName != "") {
      new VIAM.SensorClient(client, globalConfig.depthSensorName)
        .getReadings()
        .then((d) => {
          globalData.depth = d.Depth * 3.28084;
        })
        .catch((e) => {
          globalConfig.depthSensorName = "";
          errorHandler(e, "depth");
        });
    }

    if (globalConfig.windSensorName != "") {
      new VIAM.SensorClient(client, globalConfig.windSensorName)
        .getReadings()
        .then((d) => {
          if (d["Reference"] == "True (ground referenced to North)") {
            globalData.windAngle = d["Wind Angle"];
            globalData.windSpeed = d["Wind Speed"] * 1.94384;
          }
        })
        .catch((e) => {
          globalConfig.windSensorName = "";
          errorHandler(e, "wind");
        });
    }

    if (globalConfig.spotZeroFWSensorName != "") {
      new VIAM.SensorClient(client, globalConfig.spotZeroFWSensorName)
        .getReadings()
        .then((d) => {
          globalData.spotZeroFW = d["Product Water Flow"] * 0.00440287;
        })
        .catch((e) => {
          globalConfig.spotZeroFWSensorName = "";
          errorHandler(e, "spot zero fw");
        });
    }

    if (globalConfig.spotZeroSWSensorName != "") {
      new VIAM.SensorClient(client, globalConfig.spotZeroSWSensorName)
        .getReadings()
        .then((d) => {
          globalData.spotZeroSW = d["Product Water Flow"] * 0.00440287;
        })
        .catch((e) => {
          globalConfig.spotZeroSWSensorName = "";
          errorHandler(e, "spot zero sw");
        });
    }

    if (globalConfig.seakeeperSensorName != "") {
      new VIAM.SensorClient(client, globalConfig.seakeeperSensorName)
        .getReadings()
        .then((d) => {
          globalData.seakeeperData = d;
        })
        .catch((e) => {
          globalConfig.seakeeperSensorName = "";
          errorHandler(e, "seakeeper");
        });
    }

    globalConfig.acPowers.forEach((acPowerName) => {
      new VIAM.SensorClient(client, acPowerName)
        .getReadings()
        .then((d) => {
          var n = acPowerName.split("ac-")[1];
          globalData.acPowers[n] = d;
          globalData.acPowerData = true;
        })
        .catch(errorHandlerMaker(acPowerName));
    });

    if (loopNumber % 15 == 3) {
      globalConfig.vicPowerNames.forEach((powerName) => {
        new VIAM.SensorClient(client, powerName)
          .getReadings()
          .then((d) => {
            globalData.vicPowers[powerName] = d;
          })
          .catch(errorHandlerMaker(powerName));
      });

      if (globalConfig.vicDoorsSensorName != "") {
        new VIAM.SensorClient(client, globalConfig.vicDoorsSensorName)
          .getReadings()
          .then((d) => {
            globalData.vicDoors = d;
          })
          .catch(errorHandlerMaker(globalConfig.vicDoorsSensorName));
      }
    }

    if (globalConfig.routeSensorName != "") {
      new VIAM.SensorClient(client, globalConfig.routeSensorName)
        .getReadings()
        .then((raw) => {
          globalData.route = raw;
        })
        .catch(function (_e) {
          globalData.route = {};
        });
    }

    if (globalConfig.navServiceName != "") {
      new VIAM.NavigationClient(client, globalConfig.navServiceName)
        .getWayPoints()
        .then((wps) => {
          globalData.navWaypoints = (wps ?? [])
            .filter((wp) => wp.location != null)
            .map((wp) => ({
              id: wp.id,
              lat: wp.location!.latitude,
              lng: wp.location!.longitude,
            }));
        })
        .catch((e) => {
          globalData.navWaypoints = [];
          errorHandler(e, "navigation");
        });
    }

    if (loopNumber % 30 == 2) {
      globalData.allResources.forEach((r) => {
        if (r.subtype != "sensor") {
          return;
        }
        if (r.name.indexOf("fuel") < 0 && r.name.indexOf("freshwater") < 0) {
          return;
        }

        var sp = r.name.split(":");
        var name = sp[sp.length - 1];

        new VIAM.SensorClient(client, r.name)
          .getReadings()
          .then((raw) => {
            globalData.gauges[name] = raw;
          })
          .catch(errorHandlerMaker(r.name));
      });
    }

    // AIS + airstream poll at 10 s, faster than the 30 s gauge poll because
    // boat positions move continuously and the airstream feed accumulates
    // websocket messages between fetches.
    if (loopNumber % 10 == 2) {
      const aisFetches: Promise<{ boat: any; ts: number }[]>[] = [];
      if (globalConfig.aisSensorName != "") {
        aisFetches.push(fetchBoatsFromSensor(client, globalConfig.aisSensorName, "ais"));
      }
      // Airstream is gated on the layer toggle: only fetch when the user
      // has selected it, since the airstream sensor itself is idle until
      // we send it a bounding box.
      if (globalConfig.airstreamName != "" && airstreamLayerActive) {
        aisFetches.push(
          fetchBoatsFromSensor(client, globalConfig.airstreamName, "airstream")
        );
      }
      if (aisFetches.length === 0) {
        globalData.aisBoats = [];
      } else {
        Promise.all(aisFetches).then((sources) => {
          // Merge sources by mmsi, keeping the entry with the latest Timestamp.
          const byMmsi = new Map<string, { boat: any; ts: number }>();
          for (const src of sources) {
            for (const e of src) {
              const existing = byMmsi.get(e.boat.mmsi);
              if (!existing || e.ts > existing.ts) {
                byMmsi.set(e.boat.mmsi, e);
              }
            }
          }
          globalData.aisBoats = Array.from(byMmsi.values()).map((v) => v.boat);
        });
      }
    }
  }

  // fetchBoatsFromSensor pulls AIS-shaped readings from a viamboat sensor
  // (either the local `ais` model or the `aisstream` model — both return
  // the same map<mmsi, {Timestamp, Location, Heading, COG, SOG, Name, ...}>
  // shape). Returned entries carry the parsed Timestamp ms so the caller
  // can dedupe across multiple sources by recency.
  function fetchBoatsFromSensor(
    client: any,
    name: string,
    label: string
  ): Promise<{ boat: any; ts: number }[]> {
    return new VIAM.SensorClient(client, name)
      .getReadings()
      .then((raw: any) => {
        const out: { boat: any; ts: number }[] = [];
        for (const mmsi in raw) {
          const rawBoat = raw[mmsi];
          if (
            rawBoat == null ||
            typeof rawBoat !== "object" ||
            rawBoat.Location == null ||
            rawBoat.Location.length != 2 ||
            rawBoat.Location[0] == null
          ) {
            continue;
          }
          // Both sources serialize Timestamp as RFC822Z. new Date() handles
          // it; on parse failure fall back to 0 so any other source wins.
          let ts = 0;
          if (typeof rawBoat.Timestamp === "string") {
            const parsed = Date.parse(rawBoat.Timestamp);
            if (!Number.isNaN(parsed)) ts = parsed;
          }
          // Field names vary slightly between AIS sources (Cog vs COG, etc).
          // Try the common variants and skip whatever the sensor doesn't set.
          const cog =
            typeof rawBoat.Cog === "number"
              ? rawBoat.Cog
              : typeof rawBoat.COG === "number"
                ? rawBoat.COG
                : typeof rawBoat.Course === "number"
                  ? rawBoat.Course
                  : undefined;
          const sog =
            typeof rawBoat.Sog === "number"
              ? rawBoat.Sog
              : typeof rawBoat.SOG === "number"
                ? rawBoat.SOG
                : typeof rawBoat.Speed === "number"
                  ? rawBoat.Speed
                  : 0;
          const length =
            typeof rawBoat.Length === "number" && rawBoat.Length > 0
              ? rawBoat.Length
              : undefined;
          const beam =
            typeof rawBoat.Beam === "number" && rawBoat.Beam > 0
              ? rawBoat.Beam
              : typeof rawBoat.Width === "number" && rawBoat.Width > 0
                ? rawBoat.Width
                : undefined;
          const destination =
            typeof rawBoat.Destination === "string" && rawBoat.Destination.trim() !== ""
              ? rawBoat.Destination.trim()
              : undefined;
          out.push({
            boat: {
              name: rawBoat.Name || "",
              location: rawBoat.Location,
              speed: sog,
              heading: rawBoat.Heading || 0,
              cog,
              length,
              beam,
              destination,
              mmsi: mmsi,
            },
            ts,
          });
        }
        return out;
      })
      .catch((e: any) => {
        errorHandler(e, label);
        return [] as { boat: any; ts: number }[];
      });
  }

  // Send a DoCommand to the airstream sensor, no-op if not configured or no
  // client. Airstream tolerates rapid re-subscriptions but each one drops the
  // current websocket and reconnects, so callers should debounce viewport
  // changes via onAirstreamBboxChange.
  function airstreamDoCommand(cmd: Record<string, any>) {
    if (!globalClient || !globalConfig.airstreamName) return;
    new VIAM.SensorClient(globalClient, globalConfig.airstreamName)
      .doCommand(VIAM.Struct.fromJson(cmd))
      .catch((e: any) => errorHandler(e, "airstream"));
  }

  // Bridge from the map: bbox=null means the layer was toggled off (or the
  // viewport became invalid). We send clear/set accordingly. Set calls are
  // debounced so dragging/zooming doesn't churn the airstream connection.
  function onAirstreamBboxChange(
    bbox: { minLon: number; minLat: number; maxLon: number; maxLat: number } | null
  ) {
    if (airstreamBboxDebounce !== undefined) {
      clearTimeout(airstreamBboxDebounce);
      airstreamBboxDebounce = undefined;
    }
    if (bbox == null) {
      airstreamLayerActive = false;
      airstreamDoCommand({ command: "clear_bounding_box" });
      return;
    }
    airstreamLayerActive = true;
    airstreamBboxDebounce = window.setTimeout(() => {
      airstreamBboxDebounce = undefined;
      airstreamDoCommand({
        command: "set_bounding_box",
        min_lat: bbox.minLat,
        max_lat: bbox.maxLat,
        min_lon: bbox.minLon,
        max_lon: bbox.maxLon,
      });
    }, 800);
  }

  function acPowerVoltAverage(data) {
    var total = 0;
    var num = 0;

    for (var k in data) {
      var dd = data[k];
      total += dd["Line-Neutral AC RMS Voltage"];
      num++;
    }

    return total / num;
  }

  function acPowerAmpAt(vvv, data) {
    var total = 0;

    for (var k in data) {
      var dd = data[k];
      var a = dd["AC RMS Current"];
      var v = dd["Line-Neutral AC RMS Voltage"];
      var w = a * v;
      total += w / vvv;
    }

    return total;
  }

  function filterFilteredCameras(all) {
    return all.filter((r) => {
      var lookFor = r.name + "-filtered";
      var temp = all.filter((r2) => {
        return r2.name == lookFor;
      });
      return temp.length == 0;
    });
  }

  function doCameraLoop(loopNumber: number, client: VIAM.RobotClient) {
    while (globalData.lastCameraTimes.length > 20) {
      globalData.lastCameraTimes.shift();
    }

    if (globalData.lastCameraTimes.length > 0) {
      var avg =
        globalData.lastCameraTimes.reduce((a, b) => a + b) / globalData.lastCameraTimes.length;
      var mod = Math.floor((avg * 20) / 1000);

      if (mod > 0 && loopNumber > 4 && loopNumber % mod > 0) {
        return;
      }
    }

    var start = new Date();

    filterFilteredCameras(filterResources(globalData.allResources, "component", "camera")).forEach(
      (r) => {
        var cc = findComponentConfig(r.name);
        var skip = cc && cc.attributes && cc.attributes["chartplotter-hide"];

        if (skip) {
          if (removeCamera(r.name)) {
            console.log("removed camera: " + r.name);
          }
          return;
        }

        if (globalData.cameraNames.indexOf(r.name) < 0) {
          globalData.cameraNames.push(r.name);
          globalData.cameraNames.sort();
        }

        new VIAM.CameraClient(client, r.name)
          .getImages()
          .then(function (images) {
            const img = images.images[0].image;
            var ms = new Date() - start;
            globalData.lastCameraTimes.push(ms);
            var i = document.getElementById(r.name);
            if (i) {
              // Revoke old blob URL before creating new one to prevent memory leak
              if (cameraBlobUrls[r.name]) {
                URL.revokeObjectURL(cameraBlobUrls[r.name]);
              }
              const newUrl = URL.createObjectURL(new Blob([img]));
              cameraBlobUrls[r.name] = newUrl;
              i.src = newUrl;
            }
          })
          .catch((e) => {
            removeCamera(r.name);
            errorHandler(e, r.name);
          });
      }
    );
  }

  function removeCamera(n) {
    var idx = globalData.cameraNames.indexOf(n);
    if (idx >= 0) {
      globalData.cameraNames.splice(idx, 1);
      return true;
    }
    return false;
  }

  function filterResourcesFirstMatchingName(resources, t, st, n) {
    var matching = filterResources(resources, t, st, n);
    if (matching.length > 0) {
      return matching[0].name;
    }
    return "";
  }

  function filterResourcesAllMatchingNames(resources, t, st, n) {
    var matching = filterResources(resources, t, st, n);
    var names = [];
    for (var r of matching) {
      names.push(r.name);
    }
    return names.sort();
  }

  // t - type
  // st - subtype
  // n - name regex
  function filterResources(resources, t, st, n) {
    var a = [];
    for (var r of resources) {
      if ((t != "", r.type != t)) {
        continue;
      }

      if ((st != "", r.subtype != st)) {
        continue;
      }

      if (n != null && !r.name.match(n)) {
        continue;
      }

      a.push(r);
    }

    return a;
  }

  async function updateResources(client: VIAM.RobotClient) {
    var resources = await client.resourceNames();
    globalData.allResources = resources;

    const machineStatus = await client.getMachineStatus();
    globalData.machineStatus = machineStatus;
    console.log(globalData.machineStatus);

    await setupMovementSensor(client, resources);

    globalConfig.aisSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /\bais$/
    );
    globalConfig.airstreamName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /\bairstream$/
    );
    globalConfig.navServiceName = filterResourcesFirstMatchingName(
      resources,
      "service",
      "navigation",
      null
    );
    globalConfig.routeSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /\broute$/
    );
    globalConfig.seatempSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /\bseatemp$/
    );
    globalConfig.depthSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /depth/
    );
    globalConfig.windSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /wind/
    );
    globalConfig.spotZeroFWSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /spotzero-fw/
    );
    globalConfig.spotZeroSWSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /spotzero-sw/
    );
    globalConfig.seakeeperSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /seakeeper/
    );
    globalConfig.acPowers = filterResourcesAllMatchingNames(
      resources,
      "component",
      "sensor",
      /\bac-\d-\d$/
    );
    globalConfig.vicPowerNames = filterResourcesAllMatchingNames(
      resources,
      "component",
      "sensor",
      /vic-power/
    );
    globalConfig.vicDoorsSensorName = filterResourcesFirstMatchingName(
      resources,
      "component",
      "sensor",
      /vic-doors/
    );

    console.log("globalConfig", $state.snapshot(globalConfig));
  }

  async function setupMovementSensor(client: VIAM.RobotClient, resources) {
    resources = filterResources(resources, "component", "movement_sensor", null);

    var allGpsNames = [];

    // pick best movement sensor
    var bestName = "";
    var bestScore = 0;
    var bestProp = {};

    for (var r of resources) {
      const msClient = new VIAM.MovementSensorClient(client, r.name);
      var prop = await msClient.getProperties();

      var score = 0;
      if (prop.positionSupported) {
        console.log(r);
        allGpsNames.push(r.name);
        score++;
      }
      if (prop.linearVelocitySupported) {
        score++;
      }
      if (prop.compassHeadingSupported) {
        score++;
      }

      //console.log(r.name + " : " + score);

      if (score > bestScore || (score == bestScore && r.name.length < bestName.length)) {
        bestName = r.name;
        bestScore = score;
        bestProp = prop;
      }
    }

    globalConfig.movementSensorName = bestName;
    globalConfig.movementSensorProps = bestProp;
    globalConfig.movementSensorAlternates = allGpsNames;
  }

  async function updateAndLoop() {
    globalData.numUpdates++;

    var timeSinceLastError = new Date() - globalData.statusLastError;
    if (timeSinceLastError > 120 * 1000) {
      globalData.status = "";
    }

    if (!globalClient) {
      try {
        globalClient = await connect();
        await updateResources(globalClient);
      } catch (error) {
        globalData.status = "Connect failed: " + error;
        globalClient = null;
      }
    } else if (globalData.numUpdates % 120 == 0) {
      await updateResources(globalClient);
    }

    var client = globalClient;

    if (client) {
      doUpdate(globalData.numUpdates, client);
      doCameraLoop(globalData.numUpdates, client);
    }

    updateLoopTimeout = setTimeout(updateAndLoop, 1000);
    if (globalData.numUpdates == 1) {
      cloudLoopTimeout = setTimeout(updateCloudDataAndLoop, 1000);
    }
  }

  function getHostAndCredentials() {
    const urlParams = new URLSearchParams(window.location.search);

    var host = urlParams.get("host");
    var apiKey = urlParams.get("api-key");
    var authEntity = urlParams.get("authEntity");

    if (!host || host == "") {
      host = getCookie("host");
      apiKey = getCookie("api-key");
      authEntity = getCookie("api-key-id");
    }

    if (!host || host == "") {
      var machineId = window.location.pathname.split("/")[2];
      if (machineId != "") {
        var rawCookie = getCookie(machineId);
        if (rawCookie != "") {
          var x = JSON.parse(rawCookie);
          host = x.hostname;
          authEntity = x.id;
          apiKey = x.key;
        }
      }
    }

    const credential = {
      type: "api-key",
      payload: apiKey,
      authEntity: authEntity,
    };

    return [host, credential];
  }

  async function updateCloudDataAndLoop() {
    const [, credential] = getHostAndCredentials();

    if (!globalCloudClient) {
      try {
        var opts: VIAM.ViamClientOptions = {
          serviceHost: "https://app.viam.com",
          credentials: credential,
        };

        var userTokenCookie = getCookie("userToken");
        if (userTokenCookie) {
          const startIndex = userTokenCookie.indexOf("{");
          const endIndex = userTokenCookie.indexOf("}");
          userTokenCookie = userTokenCookie.slice(startIndex, endIndex + 1);

          const { access_token: accessToken } = JSON.parse(userTokenCookie);
          opts.credentials = {
            type: "access-token",
            payload: accessToken,
          };
          console.log("new credentials", opts.credentials);
        }

        globalCloudClient = await VIAM.createViamClient(opts);
      } catch (error) {
        console.log("cannot connect to cloud: " + error);
      }
    }

    if (globalCloudClient) {
      try {
        await updateMachineConfig(globalCloudClient.appClient);
        await updateGaugeGraphs(globalCloudClient.dataClient);
      } catch (error) {
        console.log("updateGaugeGraphs error: " + error);
      }
    }

    cloudLoopTimeout = setTimeout(updateCloudDataAndLoop, 1000);
  }

  async function updateMachineConfig(ac) {
    const part = await ac.getRobotPart(globalClientCloudMetaData.machinePartId);

    if (!part || !part.part) {
      throw new Error("Failed to get robot part: part or part.part is undefined");
    }

    if (part.configJson) {
      globalData.partConfig = JSON.parse(part.configJson);
    }
  }

  function findComponentConfig(n) {
    if (!globalData.partConfig) {
      return null;
    }

    if (!globalData.partConfig.components) {
      return null;
    }

    for (var i = 0; i < globalData.partConfig.components.length; i++) {
      var c = globalData.partConfig.components[i];
      if (c.name == n) {
        return c;
      }
    }
    return null;
  }

  function _isComponentMethodHot(n, method) {
    var c = findComponentConfig(n);
    if (!c) {
      return false;
    }

    var scs = c["service_configs"];
    if (!scs) {
      return false;
    }
    scs = scs.filter((x) => x["type"] == "data_manager");
    for (var i = 0; i < scs.length; i++) {
      var sc = scs[i];
      var cm = sc["attributes"]["capture_methods"];
      if (!cm) {
        continue;
      }
      var p = cm.filter((x) => x["method"] == method);
      if (p.length < 1) {
        continue;
      }
      var pp = p[0];
      if (pp["recent_data_store"] && pp["recent_data_store"]["stored_hours"] >= 24) {
        return true;
      }
    }

    return false;
  }

  // The historical sparklines all bucket readings by year-month-day
  // hour:minute and use the resulting string as the chart's _id key.
  // `minuteExpr` lets a caller swap in a coarser minute term (e.g.
  // 15-minute buckets) without rebuilding the surrounding $concat.
  function bucketIdConcat(minuteExpr?: any) {
    return {
      $concat: [
        { $toString: { $substr: [{ $year: "$time_received" }, 2, -1] } },
        "-",
        { $toString: { $month: "$time_received" } },
        "-",
        { $toString: { $dayOfMonth: "$time_received" } },
        " ",
        { $toString: { $hour: "$time_received" } },
        ":",
        minuteExpr ?? { $toString: { $minute: "$time_received" } },
      ],
    };
  }

  // Resolve the data-client scope for a component and pre-serialize a
  // standard tabular-data pipeline that starts with a $match for that
  // component. Callers pass the rest of the pipeline (group / sort /
  // limit) plus optional method_name / time_received filters.
  async function buildTabularQuery(
    componentName: string,
    pipeline: Record<string, any>[],
    opts: { methodName?: string; startTime?: Date } = {}
  ): Promise<{ scope: any; query: any[]; leaf: string }> {
    var scope = await dataClientForComponent(componentName);
    var leaf = componentName.split(":").pop() || componentName;
    var match: any = {
      location_id: scope.locationId,
      robot_id: scope.robotId,
      component_name: leaf,
    };
    if (opts.methodName) match.method_name = opts.methodName;
    if (opts.startTime) match.time_received = { $gte: opts.startTime };
    var query = [
      BSON.serialize({ $match: match }),
      ...pipeline.map((s) => BSON.serialize(s)),
    ];
    return { scope, query, leaf };
  }

  // Run a tabular-data query with optional hot→cold fallback. Logs a
  // single line per call with row count, elapsed ms, and which path
  // (hot/cold and host/remote/host-fallback-remote) handled it.
  async function runTabularQuery(
    label: string,
    scope: any,
    query: any[],
    opts: { hot?: boolean; coldFallback?: boolean } = {}
  ): Promise<any[]> {
    var hot = opts.hot !== false; // default true
    var t0 = performance.now();
    var source = hot ? "hot" : "cold";
    var data: any[] = [];
    try {
      data = await scope.dc.tabularDataByMQL(scope.orgId, query, hot);
    } catch (e: any) {
      console.log(label + ": " + source + " threw:", e?.message || String(e));
    }
    if (data.length === 0 && opts.coldFallback && hot) {
      source = "cold";
      try {
        data = await scope.dc.tabularDataByMQL(scope.orgId, query, false);
      } catch (e: any) {
        console.log(label + ": cold fallback threw:", e?.message || String(e));
      }
    }
    var elapsed = Math.round(performance.now() - t0);
    console.log(
      label +
        ": " +
        data.length +
        " rows in " +
        elapsed +
        "ms (" +
        source +
        " via " +
        scope.source +
        ")"
    );
    return data;
  }

  async function getDataViaMQL(_dc, g, startTime, shortRange) {
    var minuteExpr = shortRange
      ? { $toString: { $minute: "$time_received" } }
      : {
          $toString: {
            $multiply: [15, { $floor: { $divide: [{ $minute: "$time_received" }, 15] } }],
          },
        };
    var limit = shortRange ? 4 * 60 : 24 * 4;
    var built = await buildTabularQuery(
      g,
      [
        {
          $group: {
            _id: bucketIdConcat(minuteExpr),
            ts: { $min: "$time_received" },
            min: { $min: "$data.readings.Level" },
            max: { $max: "$data.readings.Level" },
          },
        },
        { $sort: { ts: -1 } },
        { $limit: limit },
        { $sort: { ts: 1 } },
      ],
      { startTime }
    );
    return runTabularQuery("gauge[" + g + "]", built.scope, built.query);
  }

  async function positionHistoryMQL(dc, startTime) {
    if (globalConfig.movementSensorForQuery != "") {
      var res = await positionHistoryMQLNamed(dc, startTime, globalConfig.movementSensorForQuery);
      if (res.length > 0) {
        return res;
      }
    }

    for (var i = 0; i < globalConfig.movementSensorAlternates.length; i++) {
      var n = globalConfig.movementSensorAlternates[i];

      res = await positionHistoryMQLNamed(dc, startTime, n);
      if (res.length > 0) {
        globalConfig.movementSensorForQuery = n;
        return res;
      }
    }
    return res;
  }

  function findComponentStatus(n) {
    for (var i = 0; i < globalData.machineStatus.resources.length; i++) {
      var x = globalData.machineStatus.resources[i];
      if (x.name.name == n) {
        return x.cloudMetadata;
      }
    }
    return null;
  }

  // Pull the remote-name prefix off a fully-qualified component name.
  // Viam exposes a remote's components as "<remote-name>:<component>",
  // so if the name contains a colon the segment before it is the
  // remote's name in the main part's `remotes` config block.
  function remoteNameFromComponent(componentName: string): string | null {
    var i = componentName.indexOf(":");
    return i > 0 ? componentName.substring(0, i) : null;
  }

  // The auth.entity for a Viam remote sits at remote.auth.entity (the
  // API-key ID) and the payload lives under remote.auth.credentials. In
  // this project's main part config the credentials are a *single*
  // object, not an array, so we accept either shape. The type is
  // typically "api-key"; we pass through whatever's stored.
  function extractRemoteCredential(remote: any): {
    type: string;
    payload: string;
    authEntity: string;
  } | null {
    if (!remote || !remote.auth) return null;
    var raw =
      (Array.isArray(remote.auth.credentials)
        ? remote.auth.credentials[0]
        : remote.auth.credentials) ?? null;
    if (!raw || !raw.payload) return null;
    return {
      type: raw.type || "api-key",
      payload: raw.payload,
      authEntity: remote.auth.entity || raw.authEntity || raw.entity || "",
    };
  }

  // Locate the remote entry that hosts components in the given
  // location. The remote's address embeds its location ID
  // ("<part-name>.<location-id>.viam.cloud") so matching by the
  // second dotted segment is the reliable correlation when component
  // names don't carry a "<remote>:" prefix.
  function findRemoteForLocation(locationId: string): any | null {
    var remotes = globalData.partConfig?.remotes;
    if (!Array.isArray(remotes)) return null;
    for (var r of remotes) {
      if (!r || !r.address || r.disabled) continue;
      var parts = String(r.address).split(".");
      if (parts.length >= 2 && parts[1] === locationId) return r;
    }
    return null;
  }

  // Build (or return a cached) ViamClient using credentials lifted from
  // a `remotes[]` entry. The remote section is the only place the main
  // app has the API key valid for the remote's org. Caches by the
  // remote's name so we don't spin up a new client every poll.
  async function getRemoteCloudClient(remote: any): Promise<VIAM.ViamClient | null> {
    var key = remote?.name || remote?.address;
    if (!key) return null;
    if (key in remoteCloudClients) return remoteCloudClients[key];
    var cred = extractRemoteCredential(remote);
    if (!cred) {
      console.log(
        "getRemoteCloudClient: no usable credential for remote",
        remote?.name,
        "auth shape:",
        remote?.auth
      );
      return null;
    }
    var promise = VIAM.createViamClient({
      serviceHost: "https://app.viam.com",
      credentials: {
        type: cred.type,
        payload: cred.payload,
        authEntity: cred.authEntity,
      },
    }).catch((e: any) => {
      console.log("getRemoteCloudClient: createViamClient failed for", remote?.name, e);
      delete remoteCloudClients[key];
      throw e;
    });
    remoteCloudClients[key] = promise;
    return promise;
  }

  // Resolve which DataClient + scoping IDs to use for a given
  // component's tabular-data query. For host-local components this is
  // just the global client with the host's IDs. For components on a
  // remote part, if we can build a client from the remote's
  // credentials we use that; otherwise we fall back to the global
  // client paired with the remote's IDs (works iff the host's API key
  // can read the remote's org, which is common for same-org setups).
  async function dataClientForComponent(componentName: string): Promise<{
    dc: any;
    orgId: string;
    locationId: string;
    robotId: string;
    source: "host" | "remote" | "host-fallback-remote";
  }> {
    var hostScope = {
      dc: globalCloudClient.dataClient,
      orgId: globalClientCloudMetaData.primaryOrgId,
      locationId: globalClientCloudMetaData.locationId,
      robotId: globalClientCloudMetaData.machineId,
      source: "host" as const,
    };
    var compStatus = findComponentStatus(componentName);
    if (!compStatus) return hostScope;
    var isOnRemote =
      compStatus.machineId !== globalClientCloudMetaData.machineId ||
      compStatus.locationId !== globalClientCloudMetaData.locationId;
    if (!isOnRemote) return hostScope;
    // Try the colon-prefix path first (explicit "<remote>:<name>"),
    // then fall back to matching by location-id embedded in the
    // remote's address (handles the flat-name case where the host
    // exposes the remote's components without the prefix).
    var remote: any = null;
    var remoteName = remoteNameFromComponent(componentName);
    if (remoteName) {
      var remotes = globalData.partConfig?.remotes;
      if (Array.isArray(remotes)) {
        remote = remotes.find((r: any) => r && r.name === remoteName) || null;
      }
    }
    if (!remote) {
      remote = findRemoteForLocation(compStatus.locationId);
    }
    if (remote) {
      try {
        var rc = await getRemoteCloudClient(remote);
        if (rc) {
          return {
            dc: rc.dataClient,
            orgId: compStatus.primaryOrgId,
            locationId: compStatus.locationId,
            robotId: compStatus.machineId,
            source: "remote",
          };
        }
      } catch {
        // already logged inside getRemoteCloudClient
      }
    }
    return {
      dc: globalCloudClient.dataClient,
      orgId: compStatus.primaryOrgId,
      locationId: compStatus.locationId,
      robotId: compStatus.machineId,
      source: "host-fallback-remote",
    };
  }

  async function positionHistoryMQLNamed(_dc, startTime, n) {
    var built = await buildTabularQuery(
      n,
      [
        { $sort: { time_received: -1 } },
        {
          $group: {
            _id: bucketIdConcat(),
            ts: { $min: "$time_received" },
            pos: { $first: "$data" },
          },
        },
        { $sort: { ts: -1 } },
      ],
      { methodName: "Position", startTime }
    );
    return runTabularQuery("position[" + n + "]", built.scope, built.query);
  }

  async function depthHistoryMQL(_dc, startTime) {
    var built = await buildTabularQuery(
      globalConfig.depthSensorName,
      [
        { $sort: { time_received: -1 } },
        {
          $group: {
            _id: bucketIdConcat(),
            ts: { $min: "$time_received" },
            depth: { $first: "$data.readings.Depth" },
          },
        },
        { $sort: { ts: -1 } },
      ],
      { startTime }
    );
    return runTabularQuery("depth", built.scope, built.query, { hot: false });
  }

  // 24h history at 15-min resolution → 96 points. Returns rows of
  // {_id, ts, temp} where temp is degrees Fahrenheit, matching the
  // unit used for the live readout. Hot is tried first; cold is the
  // documented home for these readings on most deployments, so the
  // fallback is on by default.
  async function seaTempHistoryMQL(_dc, startTime) {
    if (!globalConfig.seatempSensorName) return [];
    var minuteExpr = {
      $toString: {
        $multiply: [15, { $floor: { $divide: [{ $minute: "$time_received" }, 15] } }],
      },
    };
    var built = await buildTabularQuery(
      globalConfig.seatempSensorName,
      [
        {
          $group: {
            _id: bucketIdConcat(minuteExpr),
            ts: { $min: "$time_received" },
            // Average within each 15-min bucket so one noisy reading
            // doesn't spike the line. Convert from Celsius to
            // Fahrenheit outside the query.
            tempC: { $avg: "$data.readings.Temperature" },
          },
        },
        { $sort: { ts: -1 } },
        { $limit: 24 * 4 },
        { $sort: { ts: 1 } },
      ],
      { startTime }
    );
    var data = await runTabularQuery("seatemp", built.scope, built.query, {
      coldFallback: true,
    });
    return data
      .filter((d) => typeof d.tempC === "number" && !isNaN(d.tempC))
      .map((d) => ({ _id: d._id, ts: d.ts, temp: 32 + d.tempC * 1.8 }));
  }

  async function updatePositionHistory(dc, robotName, startTime) {
    if (!globalConfig.movementSensorName) {
      return;
    }
    var timeSince = new Date() - globalData.posHistoryLastCheck;
    if (timeSince < 120 * 1000) {
      return;
    }

    // HACK HACK
    const urlParams = new URLSearchParams(window.location.search);
    var data = [];
    if (
      urlParams.get("host") == "boat-main.0pdb3dyxqg.viam.cloud" &&
      urlParams.get("authEntity")[0] == "a"
    ) {
      var foo = await fetch(
        "https://us-central1-eliothorowitz.cloudfunctions.net/albertboat?d=" + startTime,
        { method: "GET" }
      );
      var text = await foo.text();
      if (!text) {
        return;
      }
      var bar = JSON.parse(text);
      data = bar.data;
    } else {
      data = await positionHistoryMQL(dc, startTime);
    }

    // Try to get depth history to color-code track
    var depthLookup = {};
    if (globalConfig.depthSensorName != "" && globalData.showDepthOnTrack) {
      try {
        var depthData = await depthHistoryMQL(dc, startTime);
        depthData.forEach((d) => {
          depthLookup[d._id] = d.depth * 3.28084; // convert to feet
        });
      } catch (e) {
        console.log("failed to get depth history", e);
      }
    }

    data = data.map((raw) => {
      // raw.ts comes from the MQL `$min: "$time_received"` aggregation. BSON
      // deserialization typically returns a Date; the legacy albertboat fetch
      // path can return a string. Normalise here so renderHistoricalTrack
      // gets a Date and can do its realtime/historical hand-off correctly.
      var ts: Date | undefined;
      if (raw.ts instanceof Date) {
        ts = raw.ts;
      } else if (typeof raw.ts === "string") {
        const parsed = new Date(raw.ts);
        if (!Number.isNaN(parsed.getTime())) ts = parsed;
      } else if (typeof raw.ts === "number") {
        ts = new Date(raw.ts);
      }
      var point: any = {
        lat: raw.pos.coordinate.latitude,
        lng: raw.pos.coordinate.longitude,
        ts,
      };
      if (raw._id && depthLookup[raw._id] !== undefined) {
        point.depth = depthLookup[raw._id];
      }
      return point;
    });

    // Limit position history to 7 days (prevents unbounded memory growth)
    const MAX_HISTORY_POINTS = 7 * 24 * 60; // 7 days * 24 hours * 60 minutes = 10,080 points
    if (data.length > MAX_HISTORY_POINTS) {
      data = data.slice(-MAX_HISTORY_POINTS);
    }

    globalData.posHistoryLastCheck = new Date();
    globalData.posHistory = data;
  }

  async function updateGaugeGraphs(dc, robotName) {
    var positionStartTime = new Date(new Date() - 86400 * 1000);
    updatePositionHistory(dc, robotName, positionStartTime);

    var gaugeWindowMs = globalData.shortGraphRange ? 4 * 3600 * 1000 : 86400 * 1000;
    var gaugeStartTime = new Date(new Date() - gaugeWindowMs);

    for (var g in globalData.gauges) {
      var h = globalData.gaugesToHistorical[g];
      if (h && new Date() - h.ts < 60000) {
        continue;
      }

      var timeStart = new Date();
      var data = await getDataViaMQL(dc, g, gaugeStartTime, globalData.shortGraphRange);
      var getDataTime = new Date().getTime() - timeStart.getTime();

      console.log(
        "time to get graph data for " +
          g +
          " took " +
          getDataTime +
          " and had " +
          data.length +
          " points"
      );

      h = { ts: new Date(), data: data };
      globalData.gaugesToHistorical[g] = h;
    }

    // Sea-temp history piggybacks on the gauge poll cycle (same 60s
    // dedupe + same window). Skip when no sensor is configured or the
    // last fetch is fresh enough.
    if (globalConfig.seatempSensorName) {
      var st = globalData.seaTempHistorical;
      if (!st || new Date() - st.ts >= 60000) {
        var seaTempData = await seaTempHistoryMQL(dc, gaugeStartTime);
        globalData.seaTempHistorical = { ts: new Date(), data: seaTempData };
      }
    }
  }

  function toggleShortGraphRange() {
    globalData.shortGraphRange = !globalData.shortGraphRange;
    var url = new URL(window.location.href);
    if (globalData.shortGraphRange) {
      url.searchParams.set("shortGraph", "1");
    } else {
      url.searchParams.delete("shortGraph");
    }
    window.history.replaceState({}, "", url);
    globalData.gaugesToHistorical = {};
  }

  async function connect(): VIAM.RobotClient {
    const [host, credential] = getHostAndCredentials();

    var c = await VIAM.createRobotClient({
      host,
      credentials: credential,
      signalingAddress: "https://app.viam.com:443",

      // optional: configure reconnection options
      reconnectMaxAttempts: 20,
      reconnectMaxWait: 5000,
    });

    globalData.status = "Connected";

    globalLogger.info("connected!");

    c.on("disconnected", disconnected);
    c.on("reconnected", reconnected);

    globalClientCloudMetaData = await c.getCloudMetadata();
    console.log(globalClientCloudMetaData);

    // Update page title with hostname
    document.title = `${host.split(".")[0]} - Viam Chartplotter`;

    return c;
  }

  async function disconnected(_event) {
    globalData.status = "Disconnected";
    globalLogger.warn("The robot has been disconnected. Trying reconnect...");
  }

  async function reconnected(_event) {
    globalData.status = "Connected";
    globalLogger.warn("The robot has been reconnected. Work can be continued.");
  }

  async function start() {
    try {
      updateAndLoop();
      return {};
    } catch (error) {
      errorHandler(error);
      console.log(error.stack);
    }
  }

  function syncFromHash() {
    globalData.showYachtDetails = window.location.hash === "#yacht-details";
  }

  onMount(() => {
    start();
    checkServerVersion();
    window.addEventListener("keydown", handleKeydown);
    window.addEventListener("hashchange", syncFromHash);
    syncFromHash();
  });

  onDestroy(() => {
    // Clear timeout loops to prevent memory leaks
    if (updateLoopTimeout !== undefined) {
      clearTimeout(updateLoopTimeout);
    }
    if (cloudLoopTimeout !== undefined) {
      clearTimeout(cloudLoopTimeout);
    }
    if (versionLoopTimeout !== undefined) {
      clearTimeout(versionLoopTimeout);
    }

    // Revoke all blob URLs to free memory
    Object.values(cameraBlobUrls).forEach((url) => {
      URL.revokeObjectURL(url);
    });
    cameraBlobUrls = {};

    // Clear enlarged image interval
    if (enlargedImageInterval) {
      clearInterval(enlargedImageInterval);
      enlargedImageInterval = null;
    }

    // Remove keydown event listener
    window.removeEventListener("keydown", handleKeydown);
    window.removeEventListener("hashchange", syncFromHash);

    // Disconnect client event listeners if present
    if (globalClient) {
      try {
        // Remove any event listeners attached to the client
        globalClient.removeAllListeners?.();
      } catch (e) {
        console.log("Error cleaning up client:", e);
      }
    }
  });

  function dicToArray(d, sortFunction) {
    var names = Object.keys(d);
    if (sortFunction) {
      names = sortFunction(names);
    } else {
      names.sort();
    }

    var a = [];

    for (var i = 0; i < names.length; i++) {
      var n = names[i];
      a.push([n, d[n]]);
    }
    return a;
  }

  // Reorganize the flat gauges dict into a sequence that puts each
  // `<type>-All` aggregate row at the bottom of its type group, marks
  // the sibling non-aggregate rows as "compact" so they render at a
  // smaller scale, and falls back to plain rendering when a type has
  // no aggregate (or has only the aggregate, with no siblings to roll
  // up). Cross-type order is alphabetical-by-type — within a type the
  // existing tankSort hints (fwd / mid / aft / main) still apply.
  function organizeGauges(gauges) {
    var byType = {};
    for (var k of Object.keys(gauges)) {
      var v = gauges[k];
      var type = (v && v.Type) || "Other";
      if (!byType[type]) byType[type] = { items: [] };
      if (/-all$/i.test(k)) {
        byType[type].agg = { key: k, value: v };
      } else {
        byType[type].items.push({ key: k, value: v });
      }
    }
    var result = [];
    for (var t of Object.keys(byType).sort()) {
      var g = byType[t];
      var aggActive = !!g.agg && g.items.length > 0;
      var sortedNames = tankSort(g.items.map((x) => x.key));
      for (var name of sortedNames) {
        var item = g.items.find((x) => x.key === name);
        result.push({
          key: item.key,
          value: item.value,
          isAggregate: false,
          isCompact: aggActive,
        });
      }
      if (g.agg) {
        result.push({
          key: g.agg.key,
          value: g.agg.value,
          isAggregate: aggActive,
          isCompact: false,
        });
      }
    }
    return result;
  }

  function gauageHistoricalToLinkedChart(data) {
    var res = {};
    for (var d in data.data) {
      var dd = data.data[d];
      res[dd._id] = Math.floor(dd.min);
    }
    return res;
  }

  // Sea-temp variant: keeps one decimal of precision (water temperature
  // typically varies in fractions of a degree, and floor-ing would
  // erase the visible detail in the sparkline).
  function seaTempToLinkedChart(data) {
    var res = {};
    for (var d in data.data) {
      var dd = data.data[d];
      res[dd._id] = Math.round(dd.temp * 10) / 10;
    }
    return res;
  }

  // Per-tank hover state, populated from LinkedChart's `hover` event.
  // Stored as a record so each tank row owns its own readout
  // independently — peer-hover is per-row, but the captured value/ts
  // needs to live somewhere reactive.
  let tankHover = $state<
    Record<string, { value: number; ts: Date } | null>
  >({});

  // Map a tank's historical _id (formatted bucket label like "26-5-11
  // 14:30") back to the bucket's start timestamp, so the hover handler
  // can show how long ago the reading was without round-tripping the
  // _id through Date.parse.
  function gaugeHistoricalTsByKey(historical: { data: any[] }): Record<string, Date> {
    const map: Record<string, Date> = {};
    for (const d of historical.data) {
      map[d._id] = d.ts instanceof Date ? d.ts : new Date(d.ts);
    }
    return map;
  }

  // Compact "X ago" formatter for the tank hover popup. Bucket sizes
  // are 1-min (short range) or 15-min (24h range), so seconds aren't
  // useful — start at minutes.
  function formatAgo(ts: Date): string {
    const ms = Date.now() - ts.getTime();
    const minutes = Math.max(0, Math.round(ms / 60000));
    if (minutes < 1) return "just now";
    if (minutes < 60) return `${minutes}m ago`;
    const hours = Math.round(minutes / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.round(hours / 24);
    return `${days}d ago`;
  }

  async function addNavWaypoint(lat: number, lng: number) {
    if (!globalClient || !globalConfig.navServiceName) {
      globalLogger.warn("no navigation service configured; cannot add waypoint");
      return;
    }
    try {
      await new VIAM.NavigationClient(globalClient, globalConfig.navServiceName).addWayPoint({
        latitude: lat,
        longitude: lng,
      });
      // Optimistically append; the next poll will reconcile with the service.
      globalData.navWaypoints = [
        ...globalData.navWaypoints,
        { id: `pending-${Date.now()}`, lat, lng },
      ];
    } catch (e) {
      errorHandler(e, "addWayPoint");
    }
  }

  async function insertNavWaypoint(beforeId: string, lat: number, lng: number) {
    if (!globalClient || !globalConfig.navServiceName) return;
    try {
      await new VIAM.NavigationClient(globalClient, globalConfig.navServiceName).doCommand(
        VIAM.Struct.fromJson({ insert_waypoint: { before_id: beforeId, lat, lng } })
      );
      // Optimistically splice; the next poll will reconcile with the service.
      const idx = beforeId
        ? globalData.navWaypoints.findIndex((wp) => wp.id === beforeId)
        : -1;
      const placeholder = { id: `pending-${Date.now()}`, lat, lng };
      if (idx >= 0) {
        globalData.navWaypoints = [
          ...globalData.navWaypoints.slice(0, idx),
          placeholder,
          ...globalData.navWaypoints.slice(idx),
        ];
      } else {
        globalData.navWaypoints = [...globalData.navWaypoints, placeholder];
      }
    } catch (e) {
      errorHandler(e, "insertWayPoint");
    }
  }

  async function moveNavWaypoint(id: string, lat: number, lng: number) {
    if (!globalClient || !globalConfig.navServiceName) return;
    if (!id || id.startsWith("pending-")) return;
    try {
      await new VIAM.NavigationClient(globalClient, globalConfig.navServiceName).doCommand(
        VIAM.Struct.fromJson({ move_waypoint: { id, lat, lng } })
      );
      // Optimistic local update; the next poll will reconcile.
      globalData.navWaypoints = globalData.navWaypoints.map((wp) =>
        wp.id === id ? { ...wp, lat, lng } : wp
      );
    } catch (e) {
      errorHandler(e, "moveWayPoint");
    }
  }

  async function removeNavWaypoint(id: string) {
    if (!globalClient || !globalConfig.navServiceName) return;
    if (!id || id.startsWith("pending-")) return;
    // Drop locally first so the marker disappears immediately.
    globalData.navWaypoints = globalData.navWaypoints.filter((wp) => wp.id !== id);
    try {
      await new VIAM.NavigationClient(globalClient, globalConfig.navServiceName).removeWayPoint(id);
    } catch (e) {
      errorHandler(e, "removeWayPoint");
    }
  }

  async function clearNavWaypoints() {
    if (!globalClient || !globalConfig.navServiceName) return;
    var nav = new VIAM.NavigationClient(globalClient, globalConfig.navServiceName);
    var existing = [...globalData.navWaypoints];
    // Snap the local state immediately so the UI feels responsive.
    globalData.navWaypoints = [];
    for (var wp of existing) {
      if (!wp.id || wp.id.startsWith("pending-")) continue;
      try {
        await nav.removeWayPoint(wp.id);
      } catch (e) {
        errorHandler(e, "removeWayPoint");
      }
    }
  }

  function seakeeper(name, value) {
    var cmd = {};
    cmd[name] = value;
    console.log("sending to: " + globalConfig.seakeeperSensorName);
    console.log(cmd);

    new VIAM.SensorClient(globalClient, globalConfig.seakeeperSensorName)
      .doCommand(VIAM.Struct.fromJson(cmd))
      .then((r) => {
        console.log(r);
      })
      .catch((e) => {
        errorHandler(e);
      });

    return true;
  }

  let enlargedImageInterval = null;

  function enlargeImage(cameraName) {
    const img = document.getElementById(cameraName);
    if (img && img.src) {
      globalData.enlargedImage = {
        name: cameraName,
        src: img.src,
      };
      if (enlargedImageInterval) clearInterval(enlargedImageInterval);
      enlargedImageInterval = setInterval(() => {
        const img = document.getElementById(cameraName);
        if (img && img.src && globalData.enlargedImage) {
          globalData.enlargedImage = {
            name: cameraName,
            src: img.src,
          };
        }
      }, 1000);
    }
  }

  function closeEnlargedImage() {
    if (enlargedImageInterval) {
      clearInterval(enlargedImageInterval);
      enlargedImageInterval = null;
    }
    globalData.enlargedImage = null;
  }

  function handleKeydown(event) {
    if (event.key === "Escape" && globalData.enlargedImage) {
      closeEnlargedImage();
    }
  }

  function toggleFullscreen() {
    if (!document.fullscreenElement) {
      document.documentElement.requestFullscreen().catch((e) => {
        errorHandler(e, "fullscreen");
      });
    } else {
      document.exitFullscreen();
    }
  }
</script>

{#if globalData.showYachtDetails}
  <div class="w-dvw min-h-dvh p-2 bg-black text-white">
    <a href="#dashboard" class="text-blue-400 hover:underline">← Back to Dashboard</a>
    <YachtDetails
      vicPowers={globalData.vicPowers}
      vicPowerNames={globalConfig.vicPowerNames}
      vicDoors={globalData.vicDoors}
    />
  </div>
{:else}
  <main
    class="w-dvw lg:h-dvh p-2 grid grid-cols-1 lg:grid-cols-4 grid-rows-3 lg:grid-rows-6 gap-2 bg-black"
  >
    <MarineMap
      myBoat={{
        name: "me",
        location: [globalData.pos.getLat(), globalData.pos.getLng()],
        speed: globalData.speed,
        heading:
          globalData.speed > 1 && globalData.cog != null
            ? globalData.cog
            : globalData.heading,
        route: globalData.route
          ? {
              destinationLongitude: globalData.route["Destination Longitude"],
              destinationLatitude: globalData.route["Destination Latitude"],
              distanceToWaypoint: globalData.route["Distance to Waypoint"],
              waypointClosingVelocity: globalData.route["Waypoint Closing Velocity"],
            }
          : undefined,
      }}
      zoomModifier={globalConfig.zoomModifier}
      boats={globalData.aisBoats}
      positionHistorical={globalData.posHistory}
      bind:depthColorTrack={globalData.showDepthOnTrack}
      depthSensorAvailable={globalConfig.depthSensorName !== ""}
      defaultAisVisible={false}
      fullWidth={globalData.hideDataPanel}
      navWaypoints={globalData.navWaypoints}
      onAddWaypoint={globalConfig.navServiceName ? addNavWaypoint : undefined}
      onMoveWaypoint={globalConfig.navServiceName ? moveNavWaypoint : undefined}
      onInsertWaypoint={globalConfig.navServiceName ? insertNavWaypoint : undefined}
      onRemoveWaypoint={globalConfig.navServiceName ? removeNavWaypoint : undefined}
      onClearWaypoints={globalConfig.navServiceName ? clearNavWaypoints : undefined}
      airstreamConfigured={globalConfig.airstreamName !== ""}
      onAirstreamBboxChange={globalConfig.airstreamName !== "" ? onAirstreamBboxChange : undefined}
      sog={globalConfig.movementSensorProps.linearVelocitySupported ? globalData.speed : null}
      hdg={globalConfig.movementSensorProps.compassHeadingSupported ? globalData.heading : null}
      cog={globalConfig.movementSensorProps.compassHeadingSupported ? globalData.cog : null}
      depth={globalConfig.depthSensorName !== "" ? globalData.depth : null}
    ></MarineMap>

    {#if !globalData.hideDataPanel}
    <aside class="lg:row-span-6 flex flex-col gap-4 border border-dark p-1 min-h-full text-white">
      {#if globalData.status === "Connected"}
        <div class="flex gap-2 items-center w-full min-h-12 px-2 border border-success-medium">
          <PrimeIcon name="broadcast" cx="text-success-dark" />
          <div class="text-sm text-success-dark">{globalData.status}</div>
        </div>
      {:else}
        <div class="flex gap-2 items-center w-full min-h-12 px-2 border border-info-medium">
          <PrimeIcon name="broadcast-off" cx="text-info-dark" />
          <div class="text-sm text-info-dark">{globalData.status}</div>
        </div>
      {/if}

      <div id="navData" class="flex flex-col divide-y">
        <!-- SOG / Depth / Heading / COG render in the map's top-right
             overlay (MarineMap's data-panel). The "color track by
             depth" toggle moved into MarineMap's layers panel — the
             $effect on showDepthOnTrack handles the cache reset
             whichever side flips it. -->
        {#if globalData.route && globalData.route["Distance to Waypoint"] > 0}
          <div class="flex gap-2 p-2 text-lg">
            <div class="min-w-32">Next Waypoint</div>
            <div>
              <div class="font-bold">
                {(globalData.route["Distance to Waypoint"] * 0.000539957).toFixed(2)} nm
              </div>
              <div class="font-bold">
                {(
                  globalData.route["Distance to Waypoint"] /
                  globalData.route["Waypoint Closing Velocity"] /
                  60
                ).toFixed(1)} minutes
              </div>
            </div>
          </div>
        {/if}

        {#if globalConfig.windSensorName != ""}
          <div class="flex gap-2 p-2 text-lg">
            <div class="min-w-32">Wind Direction</div>
            <div>
              <span class="font-bold">{globalData.windAngle.toFixed(0)}</span>
              <sup>degrees</sup>
            </div>
          </div>
          <div class="flex gap-2 p-2 text-lg">
            <div class="min-w-32">Wind Speed</div>
            <div>
              <span class="font-bold">{globalData.windSpeed.toFixed(1)}</span>
              <sup>kn</sup>
            </div>
          </div>
        {/if}

        {#if globalConfig.seatempSensorName != ""}
          <div class="p-2 text-lg">
            <div class="flex gap-2">
              <div class="min-w-32">Water Temp</div>
              <div>
                <span class="font-bold">{globalData.temp.toFixed(2)}</span>
                <sup>f</sup>
              </div>
            </div>
            {#if globalData.seaTempHistorical && globalData.seaTempHistorical.data.length >= 5}
              {@const stData = seaTempToLinkedChart(globalData.seaTempHistorical)}
              {@const stTsByKey = gaugeHistoricalTsByKey(globalData.seaTempHistorical)}
              <!-- Auto-fit the y-range to the actual reading window so a
                   small variation (~5°F) reads as a real curve rather
                   than a flat line near the chart's top. Pad ±0.5°F so
                   the line never touches the edges. -->
              {@const stValues = Object.values(stData) as number[]}
              {@const stMin = Math.floor(Math.min(...stValues) - 0.5)}
              {@const stMax = Math.ceil(Math.max(...stValues) + 0.5)}
              {@const stViewWidth = Math.max(100, Object.keys(stData).length * 4 + 4)}
              <div class="relative mt-1">
                <div
                  role="article"
                  tabindex="-1"
                  class="peer tank-chart bg-dark rounded hover:cursor-pointer overflow-hidden"
                >
                  <LinkedChart
                    data={stData}
                    style="width: 100%;"
                    width={stViewWidth}
                    height={26}
                    type="line"
                    lineColor="#fb923c"
                    fill="#fb923c"
                    scaleMin={stMin}
                    scaleMax={stMax}
                    linked="seatemp"
                    uid="seatemp"
                    barMinWidth="3"
                    grow
                    dispatchEvents={true}
                    on:hover={(e) => {
                      const ts = stTsByKey[e.detail.key];
                      if (ts) {
                        tankHover["seatemp"] = { value: e.detail.value, ts };
                      }
                    }}
                    on:value-update={(e) => {
                      if (e.detail.value == null) tankHover["seatemp"] = null;
                    }}
                  />
                </div>
                {#if tankHover["seatemp"]}
                  <div
                    class="z-10 absolute -top-1 right-1 -translate-y-full flex items-baseline gap-1.5 px-2.5 py-1 rounded-md bg-black/85 text-white text-xs shadow-lg pointer-events-none whitespace-nowrap tabular-nums"
                  >
                    <span class="font-semibold text-amber-300"
                      >{tankHover["seatemp"]!.value.toFixed(1)}°F</span
                    >
                    <span class="text-gray-400"
                      >{formatAgo(tankHover["seatemp"]!.ts)}</span
                    >
                  </div>
                {/if}
              </div>
            {/if}
          </div>
        {/if}
        <div class="flex gap-2 p-2 text-lg">
          <div class="min-w-32">Location</div>
          <span><small>{globalData.posString}</small></span>
        </div>
        <!-- Heading / COG also moved to MarineMap's data panel — same
             reason as SOG / Depth above. -->
        {#if globalConfig.spotZeroFWSensorName != ""}
          <div class="flex gap-2 p-2 text-lg">
            <div class="min-w-32">SpotZero F/S</div>
            <div>
              <span class="font-bold">{@html globalData.spotZeroFW.toFixed(2)}</span> /
              <span class="font-bold">{@html globalData.spotZeroSW.toFixed(2)}</span>
              gpm
            </div>
          </div>
        {/if}
        {#if globalConfig.seakeeperSensorName != ""}
          <div class="flex gap-2 p-2 text-lg">
            <div class="min-w-32">Seakeeper</div>
            <div>
              <span class="font-bold">
                {#if globalData.seakeeperData["power_enabled"] >= 1}
                  <button onclick={() => seakeeper("power", false)}>P</button>
                {:else if globalData.seakeeperData["power_available"] >= 1}
                  <button onclick={() => seakeeper("power", true)}>p</button>
                {/if}
                {#if globalData.seakeeperData["stabilize_enabled"] >= 1}
                  <button onclick={() => seakeeper("enable", false)}>E</button>
                {:else if globalData.seakeeperData["stabilize_available"] >= 1}
                  <button onclick={() => seakeeper("enable", true)}>e</button>
                {/if}
                {@html globalData.seakeeperData["progress_bar_percentage"].toFixed(2)}% ({globalData
                  .seakeeperData["flywheel_speed"]})
              </span>
            </div>
          </div>
        {/if}
        <div class="flex flex-col divide-y">
          {#each organizeGauges(globalData.gauges) as { key, value, isAggregate, isCompact }}
            {@const gallons = (value.Capacity * value.Level * 0.264172) / 100}
            {@const capacityGal = value.Capacity * 0.264172}
            {@const pct = value.Level}
            {@const levelClass =
              pct < 15
                ? "text-red-400"
                : pct < 30
                  ? "text-amber-300"
                  : "text-sky-300"}
            <!-- Display name: the aggregate row strips the "-All" suffix
                 since the visual treatment (border accent below + bold
                 name) already signals it's the rollup. -->
            {@const displayName = isAggregate ? key.replace(/-all$/i, "") : key}
            <!-- Tank row: header line (name on the left, % + volume on the
                 right) above a full-width sparkline. Header columns are
                 fixed-width and right-aligned so the percent and volume
                 numbers stack at the same x across every row, regardless
                 of how long the tank name is. Sparkline gets the whole
                 panel width.
                 - isCompact: this row is one of N siblings of an
                   aggregate, so render it smaller to make the cluster
                   read like a sub-group.
                 - isAggregate: the "-All" rollup for this type — render
                   at full size with a stronger top border so it visually
                   anchors the bottom of the group. -->
            <section
              class="overflow-visible px-2"
              class:py-1.5={!isCompact}
              class:py-0.5={isCompact}
              class:tank-aggregate={isAggregate}
            >
              <div
                class="flex items-baseline gap-3"
                class:mb-1={!isCompact}
                class:mb-0.5={isCompact}
              >
                <h2
                  class="capitalize flex-1 truncate"
                  class:text-sm={!isCompact}
                  class:text-xs={isCompact}
                  class:font-medium={!isAggregate}
                  class:font-semibold={isAggregate}
                  class:text-gray-200={!isAggregate}
                  class:text-sky-200={isAggregate}>{displayName}</h2
                >
                <span
                  class={`font-bold tabular-nums w-12 text-right ${levelClass}`}
                  class:text-base={!isCompact}
                  class:text-sm={isCompact}
                  >{pct.toFixed(0)}%</span
                >
                <span
                  class="text-gray-300 tabular-nums w-28 text-right"
                  class:text-sm={!isCompact}
                  class:text-xs={isCompact}
                >
                  {gallons.toFixed(0)} / {capacityGal.toFixed(0)}
                  <span class="text-gray-500 text-xs">gal</span>
                </span>
              </div>
              {#if globalData.gaugesToHistorical[key] && globalData.gaugesToHistorical[key].data.length >= 5}
                {@const tankData = gauageHistoricalToLinkedChart(globalData.gaugesToHistorical[key])}
                {@const tsByKey = gaugeHistoricalTsByKey(globalData.gaugesToHistorical[key])}
                <!-- viewBox width has to grow with bucket count or the
                     right-aligned bars overflow past x=0 and only the
                     tail fits in view. Each bucket needs at least
                     barMinWidth + gap = 4 units; +4 of slack keeps the
                     last bar clear of the right edge. The CSS
                     `width: 100%` still stretches the SVG to fill the
                     panel; this just changes the internal coordinate
                     scale so all data lands inside the viewBox. -->
                {@const chartViewWidth = Math.max(100, Object.keys(tankData).length * 4 + 4)}
                <div class="relative">
                  <div
                    role="article"
                    tabindex="-1"
                    class="peer tank-chart bg-dark rounded hover:cursor-pointer overflow-hidden"
                  >
                    <LinkedChart
                      data={tankData}
                      style="width: 100%;"
                      width={chartViewWidth}
                      height={isCompact ? 16 : 26}
                      type="line"
                      lineColor="#38bdf8"
                      fill="#38bdf8"
                      scaleMax={100}
                      linked={key}
                      uid={key}
                      barMinWidth="3"
                      grow
                      dispatchEvents={true}
                      on:hover={(e) => {
                        const ts = tsByKey[e.detail.key];
                        if (ts) {
                          tankHover[key] = { value: e.detail.value, ts };
                        }
                      }}
                      on:value-update={(e) => {
                        if (e.detail.value == null) tankHover[key] = null;
                      }}
                    />
                  </div>
                  <!-- Hover readout: floats above the chart's top-right.
                       Gated on tankHover[key] (populated by the chart's
                       hover event) instead of peer-hover so the contents
                       can never appear empty. z-10 keeps it above
                       sibling tank rows. -->
                  {#if tankHover[key]}
                    <div
                      class="z-10 absolute -top-1 right-1 -translate-y-full flex items-baseline gap-1.5 px-2.5 py-1 rounded-md bg-black/85 text-white text-xs shadow-lg pointer-events-none whitespace-nowrap tabular-nums"
                    >
                      <span class="font-semibold text-sky-300"
                        >{tankHover[key]!.value}%</span
                      >
                      <span class="text-gray-400"
                        >{formatAgo(tankHover[key]!.ts)}</span
                      >
                    </div>
                  {/if}
                </div>
              {/if}
            </section>
          {/each}
        </div>
        {#if globalData.acPowerData}
          <div class="flex gap-2 p-2">
            <div class="min-w-32">AC Power</div>
            <div style="font-size:.7em;">
              <table class="text-white">
                <tbody>
                  <tr>
                    <td></td>
                    <th>Voltage</th>
                    <th>Current</th>
                  </tr>
                  {#each dicToArray(globalData.acPowers) as [name, d]}
                    <tr>
                      <th>{name}</th>
                      <td>{d["Line-Neutral AC RMS Voltage"]}</td>
                      <td>{d["AC RMS Current"]}</td>
                    </tr>
                  {/each}
                  <tr>
                    <th>Ttl</th>
                    <td>{acPowerVoltAverage(globalData.acPowers).toFixed(0)}</td>
                    <td
                      >{acPowerAmpAt(
                        acPowerVoltAverage(globalData.acPowers),
                        globalData.acPowers
                      ).toFixed(0)}</td
                    >
                  </tr>
                  <tr>
                    <th>Ttl</th>
                    <td>{(2 * acPowerVoltAverage(globalData.acPowers)).toFixed(0)}</td>
                    <td
                      >{acPowerAmpAt(
                        2 * acPowerVoltAverage(globalData.acPowers),
                        globalData.acPowers
                      ).toFixed(0)}</td
                    >
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        {/if}
      </div>

      <div class="grow text-xs flex flex-col-reverse text-gray-500 text-right">
        {globalData.numUpdates}
      </div>
    </aside>
    {/if}

    {#if !globalData.hideDataPanel}
      <div class="h-[50dvh] lg:h-[auto] overflow-x-auto flex lg:col-span-3 border border-dark p-1">
        {#each globalData.cameraNames as name}
          <img
            id={name}
            class="w-full lg:w-[250px] cursor-pointer hover:opacity-80 transition-opacity"
            alt={name}
            onclick={() => enlargeImage(name)}
          />
        {/each}
      </div>
    {/if}

    <div class="flex flex-col gap-3 text-white text-sm">
      <div>
        <h3>Powered By</h3>
        <img
          src="/viam-logo.png"
          width="250"
          height="49"
          alt="viam logo"
          style="filter: invert(1);"
        />
      </div>
      <div class="flex items-center gap-6 flex-wrap">
        {#if globalConfig.vicPowerNames.length > 0}
          <a href="#yacht-details" class="text-blue-400 hover:underline">Yacht Details →</a>
        {/if}
        <label class="flex items-center gap-2 cursor-pointer">
          <input
            type="checkbox"
            checked={globalData.shortGraphRange}
            onchange={toggleShortGraphRange}
          />
          <span>show last 4 hours in graphs</span>
        </label>
      </div>
    </div>

    <button
      onclick={toggleFullscreen}
      class="fixed top-2 right-2 z-[10000] px-3 py-1 bg-black bg-opacity-60 border border-gray-500 hover:bg-gray-700 text-white rounded text-sm"
      title="Toggle fullscreen"
    >
      ⛶
    </button>

    <button
      onclick={toggleHideDataPanel}
      class="fixed top-2 right-12 z-[10000] px-3 py-1 bg-black bg-opacity-60 border border-gray-500 hover:bg-gray-700 text-white rounded text-sm"
      title={globalData.hideDataPanel ? "Show data panel" : "Hide data panel"}
    >
      {globalData.hideDataPanel ? "◀" : "▶"}
    </button>

    {#if globalData.enlargedImage}
      <div
        class="fixed inset-0 bg-black bg-opacity-80 flex items-center justify-center z-[9999]"
        onclick={closeEnlargedImage}
      >
        <div
          class="relative w-[95vw] h-[95vh] flex items-center justify-center"
          onclick={(e) => e.stopPropagation()}
        >
          <button
            class="absolute -top-10 right-0 text-white text-2xl hover:text-gray-300"
            onclick={closeEnlargedImage}
          >
            ✕
          </button>
          <img
            src={globalData.enlargedImage.src}
            alt={globalData.enlargedImage.name}
            class="max-w-full max-h-full object-contain"
          />
          <div class="absolute bottom-0 left-0 bg-black bg-opacity-50 text-white px-2 py-1 text-sm">
            {globalData.enlargedImage.name}
          </div>
        </div>
      </div>
    {/if}
  </main>
{/if}

<style>
  /* svelte-tiny-linked-charts renders a 1px polyline for line charts and
     gives no stroke-width prop. Style it directly so the sparkline reads
     a bit bolder against the dark panel background. :global() is needed
     because the SVG is rendered inside the third-party component and
     wouldn't otherwise pick up our scoped styles. */
  .tank-chart :global(svg polyline) {
    stroke-width: 1.5;
    stroke-linejoin: round;
    stroke-linecap: round;
  }
  /* Smooth out the hover dot so it doesn't look pixel-y. */
  .tank-chart :global(svg circle) {
    transition: r 100ms ease-out;
  }

  /* Aggregate row ("X-All") sits at the bottom of its type cluster.
     A heavier top border + faint sky tint makes the rollup visually
     distinct from the compact siblings above it without needing a
     separate header element. */
  .tank-aggregate {
    border-top: 1px solid rgba(56, 189, 248, 0.35);
    background: rgba(56, 189, 248, 0.04);
  }
</style>
