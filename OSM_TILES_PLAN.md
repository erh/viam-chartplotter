# Self-hosted OSM raster tiles

A pure-Go pipeline to render our own OSM raster tiles in this module.
Water polygons (`natural=water`, `place=sea|ocean`, `landuse=reservoir|basin`,
etc.) are kept and rendered as a **transparent carve** through the yellow
land base, so the chart renderer's depth shading shows through wherever
OSM says it's water. Whole-planet coverage. Not byte-identical to
tile.openstreetmap.org, but visually equivalent at chart-use distance,
using openstreetmap-carto's palette and zoom thresholds as the spec.

## Pipeline

```
  planet.osm.pbf  ──[ingest]──►  feature stream  ──[index]──►  tile store
                                                                   │
                                                                   ▼
  HTTP /tiles/z/x/y.png  ◄──[render]──  features for (z,x,y)
                                  │
                                  ▼
                          OSMTileCache (existing)
```

Offline: `ingest` + `index` (run on data refresh).
Online: `render` HTTP handler inside this Go module.

## Versions

### v0.1 — geometry + point labels, Latin only (current)

Smallest end-to-end slice that produces recognisable tiles.

- Ingest: stream PBF, filter to kept tag set, drop all water-related tags.
- Index: per-(z,x,y) feature blobs, SQLite-backed.
- Render: `fogleman/gg` with osm-carto palette. Roads, buildings,
  landuse, admin lines.
- Labels: place=* point labels only. `golang.org/x/image/font` with a
  bundled Latin font. No text shaping, no line labels, no collision
  buffer across tile edges yet (deterministic placement only).
- Coverage for dev iteration: small extract (e.g. Monaco, ~500KB) until
  the pipeline works, then scale up to a regional extract, then planet.

### v0.2 — multipolygon relations, labels, shaped text

- **v0.2a (done)**: multipolygon relation support. Two-pass PBF load
  (relations → member-way IDs, then nodes/ways with geometry stashing),
  then greedy endpoint stitching to assemble outer rings. Inner rings
  (holes) deferred — we drop water anyway so holes-inside-parks render
  the same regardless. Gets Riverside / Central / Battery / Prospect
  parks visible.
- **v0.2b**: `go-text/typesetting` for HarfBuzz-equivalent shaping.
  Adds CJK, Arabic, Devanagari label rendering.
- **v0.2c**: line labels along `highway=*` (curved text along path).
  Halos (white stroke under black fill).
- **v0.2d**: cross-tile label collision — 128px buffer, deterministic
  placement seeded by `hash(feature_id, zoom)`. Adjacent tiles agree.

### v0.3 — long-tail polish

- Bidi text, RTL, vertical CJK.
- POI icons.
- Area labels (parks, landuse with names).
- Performance: parallel render, mmap index, glyph cache.

## Filter rules — what we keep, what we drop

**Ingest keeps everything.** Every tagged node, every tagged way, and every
multipolygon relation lands in Mongo with its full tag map preserved on
the document. Class is pre-computed at ingest for the render fast path,
but adding a new render rule (or changing an existing one) is a code +
re-classify pass, **never a PBF re-ingest**. The only things filtered out
are coordinate-only nodes (geometry references for ways) and non-
multipolygon relations like bus routes and boundary chains, which carry no
geometry that the area renderer can use.

Classes recognised by `Classify` today (others fall through to `ClassSkip`
but are still stored):

- `ClassRoad` — `highway=*`
- `ClassBuilding` — `building=*`
- `ClassLanduse` — `landuse=*`
- `ClassLeisure` — `leisure=*`
- `ClassNatural` — `natural=wood|peak|cliff|tree|...`
- `ClassWater` — `natural=water|coastline|bay|strait|wetland`,
  `waterway=*`, `place=sea|ocean`, `landuse=reservoir|basin|salt_pond`,
  `leisure=swimming_pool|water_park|marina|swimming_area`.
  Rendered as a transparent carve through the yellow base; see
  `carveWater` in `osmtiler/render.go`.
- `ClassPlace` — `place=country|city|town|village|hamlet|island|locality`
- `ClassPOI` — `amenity`, `shop`, `tourism`, `historic`, `man_made` (except
  `pier|breakwater|groyne`, which are deliberately suppressed because they
  straddle the chart's water boundary)
- `ClassAdmin` — `boundary=administrative`
- `ClassRailway` — `railway=*`
- `ClassAeroway` — `aeroway=*`

## Index format

SQLite file: `tiles.db`.

```sql
CREATE TABLE tiles (
  z INTEGER, x INTEGER, y INTEGER,
  data BLOB,
  PRIMARY KEY (z, x, y)
);
```

`data` is our own packed format:

```
header:
  uint32 magic = 0x4F534D54  // "OSMT"
  uint16 version
  uint16 layer_count

per layer:
  uint8  layer_kind         // road, building, landuse, label, ...
  uint16 feature_count
  per feature:
    uint8  geom_kind        // point, line, polygon
    uint8  tag_count
    [tag_count] (uint16 key_idx, uint16 val_idx)  // into tile-local dict
    geometry:
      points: var-encoded (dx, dy) deltas in tile-local pixel space
        (8192 = tile width, allows sub-pixel precision)
```

Geometry is pre-clipped to tile bounds + 64px buffer (for label
overdraw at v0.2). Coords are tile-local int16 to keep blobs small.

Per-zoom simplification at ingest:
- z ≤ 8: aggressive (Douglas–Peucker tolerance = 8 pixels)
- z = 9–13: tolerance = 2 pixels
- z = 14: tolerance = 0.5 pixels (the storage zoom)
- z = 15–19: render-time only, reuse z=14 features

## Style — Go port of osm-carto

Translate `style/*.mss` into a `[]LayerRule` table in
`osm_tile_style.go`:

```go
type LayerRule struct {
    Match     TagMatcher    // e.g. highway=motorway
    MinZoom   uint8
    MaxZoom   uint8
    ZOrder    int
    Stroke    color.Color
    Fill      color.Color
    Width     ZoomWidth     // width as f(zoom)
    Dash      []float64
}
```

~200–300 rules. Drawn in z-order: landuse fills → road casings →
road fills → buildings → admin lines → labels.

## Labels — v0.1 design

Extracted at ingest as a separate layer (`layer_kind = label`).
Each label feature carries:

- anchor point (tile-local pixel coords)
- name (UTF-8, but rendered Latin-only in v0.1)
- priority (place=country=0, state=1, city=2, town=4, ..., POI=12)
- min_zoom (from osm-carto rules)

Render-time placement:

1. Sort by (priority, feature_id) for determinism.
2. For each label: try 8 positions around anchor (centered above first,
   then N, NE, NW, E, W, SE, SW).
3. Maintain per-tile R-tree of placed bboxes. First non-colliding wins.
4. Skip if no position fits.

v0.1 known limitations (deferred to v0.2):
- A label whose center sits in tile A but whose text wraps into tile B
  will be clipped at the tile edge. Cross-tile collision needs the
  buffer pass.
- Roads have no labels.
- Non-Latin scripts render as tofu (no font fallback yet).

## Integration

Code lives in the `osmtiler/` package:

- `osmtiler/filter.go`: `Classify(tags) Class` — the keep/drop choke point.
- `osmtiler/geom.go`: Web Mercator projection, tile↔lon/lat math, `Feature` / `FeatureSet`.
- `osmtiler/load.go`: `LoadPBF(ctx, path) *FeatureSet` — single-pass PBF → in-memory features.
- `osmtiler/render.go`: `RenderTile(fs, z, x, y) []byte` — paints one PNG.
- `osmtiler/zip10024_test.go`: visual checkpoint (`OSM_NYC_PBF=… go test`).

Coming:

- `cmd/osm-index`: filtered features → `tiles.db` (replaces the
  in-memory `FeatureSet` at planet scale).
- `osmtiler` HTTP handler mounting `/osm/{z}/{x}/{y}.png`.
- Switch `osm_cache.go:90` from `https://tile.openstreetmap.org/...`
  to a configurable base URL; production points at our own handler on
  localhost. Disk cache layer in `osm_cache.go` is unchanged.

## Open questions

- Where does `tiles.db` live in prod? It's ~30–60GB for the planet.
  Likely a separate volume, path configured via the module config.
- Do we want PBF refresh on a schedule, or manual? Geofabrik publishes
  diffs daily; full reingest is ~6–12 hrs of compute.
- Fonts: ship Noto Sans inside the binary (~5MB) or expect it from the
  system? Embedded is more portable.
- Should `osm-ingest` and `osm-index` be one tool or two? One is
  simpler; splitting lets us reuse the filtered stream for other things
  (e.g. label-only re-extract).
