
ifneq (,$(wildcard test.make))
	include test.make
    export $(shell sed 's/=.*//' test.make)
endif

# Go sources the module binary depends on: every non-test .go in the library
# packages (root, weather/, render/, mapdata/, …) plus cmd/module — but NOT the
# other cmd/ CLIs (datasync, tileserver, ecmwf-probe, mapsync, …), which are
# separate binaries the module doesn't import. (Previously the prereqs were
# `*.go cmd/module/*.go` — root-only — so moving code into subpackages silently
# stopped triggering rebuilds.)
GO_SRC := $(shell find . -name '*.go' -not -name '*_test.go' \
            -not -path './cmd/*' -not -path './mapdata/cmd/*') \
          $(shell find ./cmd/module -name '*.go' -not -name '*_test.go')

module: bin/viamchartplottermodule dist/index.html
	tar czf module.tar.gz bin/viamchartplottermodule meta.json dist

run: dist/index.html  Makefile
	go run cmd/run/cmd-run.go

src/output.css: node_modules src/app.css
		npm run build:css

dist/index.html: src/output.css *.json src/*.css src/*.ts src/*.svelte src/lib/*.ts node_modules
	NODE_ENV=development npm run build

lint: node_modules
	gofmt -w .
	npm run format
	npm run lint-fix
	npx svelte-check --tsconfig ./tsconfig.json

bin/viamchartplottermodule: bin $(GO_SRC) go.mod go.sum Makefile dist/index.html
	go build -o bin/viamchartplottermodule cmd/module/cmd.go

updaterdk:
	go get go.viam.com/rdk@latest
	go mod tidy


bin:
	-mkdir bin

node_modules: package.json
	npm install

setup-linux:
	which npm > /dev/null 2>&1 || apt -y install nodejs


# -- self-hosted OSM tiles: ZIP 10024 visual checkpoint -----------------
# Downloads the BBBike NewYork extract on first run, renders every zoom
# 0..18 of the tiles covering the Upper West Side, then pops open the
# output directory so you can scroll the PNGs.

OSM_NYC_PBF ?= /tmp/NewYork.osm.pbf
OSM_NYC_URL ?= https://download.bbbike.org/osm/bbbike/NewYork/NewYork.osm.pbf
OSM_TILES_OUT ?= /tmp/osm-zip10024-tiles
OPEN ?= open

$(OSM_NYC_PBF):
	curl -L -o $@ $(OSM_NYC_URL)

.PHONY: osm10024test
osm10024test: $(OSM_NYC_PBF)
	OSM_NYC_PBF=$(OSM_NYC_PBF) OSM_TILES_OUT=$(OSM_TILES_OUT) \
		go test -run TestRenderZip10024 -v ./mapdata/osmtiler -timeout 5m
	$(OPEN) $(OSM_TILES_OUT)/index.html


# -- ingest map data into MongoDB -------------------------------------------
# Parse/sync OSM + NOAA ENC data into the shared Mongo database the renderer
# reads (osm_* + noaa collections). Override MONGO / MONGO_DB on the CLI, e.g.
#   make ingest-noaa MONGO=mongodb://localhost:27017
MONGO     ?= mongodb://erh-23big.local:27017
MONGO_DB  ?= osm

# Base OS cache dir, matching Go's os.UserCacheDir() so these targets point at
# the same place the running module/services read & write:
#   macOS: ~/Library/Caches   Linux: $XDG_CACHE_HOME or ~/.cache
ifeq ($(shell uname -s),Darwin)
CACHE_DIR ?= $(HOME)/Library/Caches
else
CACHE_DIR ?= $(if $(XDG_CACHE_HOME),$(XDG_CACHE_HOME),$(HOME)/.cache)
endif
OSM_CACHE ?= $(CACHE_DIR)/viam-chartplotter/osm
ENC_CACHE ?= $(CACHE_DIR)/viam-chartplotter/noaa-enc/cells

# Extra flags forwarded to `mapsync ingest`. Pass --force to re-ingest regions
# whose PBF is unchanged but the ingest CODE changed (the dedup is by PBF hash,
# so a code change alone is otherwise skipped) — needed after an ingest-logic
# change like the admin-boundary support. Tune --workers N for parallelism vs.
# memory on large planet-scale batches (each worker buffers a whole PBF).
#   make ingest-osm-all MONGO=… INGEST_FLAGS="--force --workers 3"
INGEST_FLAGS ?=

# Atlantic-seaboard states, Geofabrik us-<state> naming. $(wildcard ...) keeps
# only the extracts actually downloaded.
EASTCOAST_STATES = maine new-hampshire massachusetts rhode-island connecticut \
	new-york new-jersey delaware maryland district-of-columbia virginia \
	north-carolina south-carolina georgia florida
EASTCOAST_PBFS = $(wildcard $(foreach s,$(EASTCOAST_STATES),$(OSM_CACHE)/us-$(s).osm.pbf))
ALL_OSM_PBFS   = $(wildcard $(OSM_CACHE)/*.osm.pbf)

.PHONY: mapsync render-cmd ingest-noaa ingest-osm-eastcoast ingest-osm-all ingest-all backfill-geomlow backfill-lowzoom

# Always rebuild the CLI so an ingest never runs against a stale binary.
mapsync:
	go build -o mapsync ./mapdata/cmd/mapsync

# Render tiles for a lat/lon straight from Mongo to disk (fast debug loop):
#   ./render-cmd --lat 32.79 --lon -79.86 --zoom 13
render-cmd:
	go build -o render-cmd ./cmd/render

# Standalone service binaries (non-Viam): map+weather server, and the two
# populate daemons that keep Mongo current.
.PHONY: tileserver datasync weathersync
tileserver:
	go build -o tileserver ./cmd/tileserver
datasync:
	go build -o datasync ./cmd/datasync
weathersync:
	go build -o weathersync ./cmd/weathersync

# Parse every downloaded ENC cell (.000) into the `noaa` collection. Upserts in
# place, so it's safe to re-run after a parser change (find|xargs handles the
# thousands-of-cells arg list).
ingest-noaa: datasync
	find $(ENC_CACHE) -name '*.000' -print0 | xargs -0 ./datasync --mongo $(MONGO) --db $(MONGO_DB)

# Ingest the downloaded Atlantic-coast state extracts into the osm_* collections.
ingest-osm-eastcoast: mapsync
	./mapsync ingest $(INGEST_FLAGS) --mongo $(MONGO) --db $(MONGO_DB) $(EASTCOAST_PBFS)

# Ingest every downloaded OSM extract (all states + countries in the cache).
ingest-osm-all: mapsync
	./mapsync ingest $(INGEST_FLAGS) --mongo $(MONGO) --db $(MONGO_DB) $(ALL_OSM_PBFS)

# Everything: all OSM extracts then all ENC cells.
ingest-all: ingest-osm-all ingest-noaa

# One-time migration: add simplified geomLow to existing osm_overview/osm_coastal
# docs so the z7..z11 land-cover band renders fast (no PBF re-ingest needed).
# New ingests write geomLow automatically; this backfills what's already there.
backfill-geomlow: mapsync
	./mapsync backfill-geomlow --mongo $(MONGO) --db $(MONGO_DB)

# One-time migration: build the curated osm_lowzoom collection (the z7/z8 band's
# small, pre-simplified feature set) from existing osm_overview/osm_coastal docs.
# New ingests populate it automatically; this backfills what's already there.
# Run AFTER backfill-geomlow so it copies the simplified geometry.
backfill-lowzoom: mapsync
	./mapsync backfill-lowzoom --mongo $(MONGO) --db $(MONGO_DB)
