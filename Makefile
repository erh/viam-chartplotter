
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

# Run the whole test suite: Go unit tests plus the frontend (Vitest) tests.
.PHONY: test
test: node_modules
	go test ./...
	npm test

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

# Build the curated noaa_lowzoom collection (the z7..z10 overview band, stored
# with valid-simplified geometry) from the existing noaa docs — no ENC re-ingest.
# Makes the overview-tile NOAA query fast (z7/z10 ~2.6s -> ~0.7s). Re-run after a
# NOAA sync to refresh it; the renderer falls back to the full noaa collection
# when it's absent.
backfill-noaa-lowzoom: mapsync
	./mapsync backfill-noaa-lowzoom --mongo $(MONGO) --db $(MONGO_DB)


# -- mobile (Flutter) app ----------------------------------------------------
# Run/build the native chartplotter in mobile/ (see mobile/README.md). Needs
# Flutter (version pinned in mobile/.fvmrc); override FLUTTER to pin via FVM:
#   make mobile-run FLUTTER="fvm flutter"
# Target a specific device with DEVICE=<id from `flutter devices`>. Boat/login
# config is forwarded as --dart-define only when set, so bare `make mobile-run`
# is chart-only (hosted tiles, no boat):
#   make mobile-run                                              # chart-only
#   make mobile-run VIAM_HOST=… VIAM_API_KEY_ID=… VIAM_API_KEY=…  # a boat
#   make mobile-run VIAM_OAUTH_ISSUER=… VIAM_OAUTH_CLIENT_ID=…    # app.viam.com login
FLUTTER ?= flutter
# Reverse-domain org for the generated platform projects (bundle id = <org>.mobile).
# Pinned so `flutter create` is deterministic — otherwise stale platform folders
# generated with different orgs make it fail with "Ambiguous organization".
MOBILE_ORG ?= com.viam
MOBILE_DEVICE := $(if $(DEVICE),-d $(DEVICE))
MOBILE_DART_DEFINES := \
	$(if $(VIAM_HOST),--dart-define=VIAM_HOST=$(VIAM_HOST)) \
	$(if $(VIAM_API_KEY_ID),--dart-define=VIAM_API_KEY_ID=$(VIAM_API_KEY_ID)) \
	$(if $(VIAM_API_KEY),--dart-define=VIAM_API_KEY=$(VIAM_API_KEY)) \
	$(if $(TILE_BASE),--dart-define=TILE_BASE=$(TILE_BASE)) \
	$(if $(DEPTH_SENSOR),--dart-define=DEPTH_SENSOR=$(DEPTH_SENSOR)) \
	$(if $(VIAM_OAUTH_ISSUER),--dart-define=VIAM_OAUTH_ISSUER=$(VIAM_OAUTH_ISSUER)) \
	$(if $(VIAM_OAUTH_CLIENT_ID),--dart-define=VIAM_OAUTH_CLIENT_ID=$(VIAM_OAUTH_CLIENT_ID)) \
	$(if $(VIAM_OAUTH_REDIRECT),--dart-define=VIAM_OAUTH_REDIRECT=$(VIAM_OAUTH_REDIRECT))

.PHONY: mobile-setup mobile-run mobile-apk mobile-analyze mobile-test

# Sentinel for the platform setup. Rebuilt only when its inputs change — the
# pinned Flutter version, pubspec, or the platform-config scripts — so a plain
# `make mobile-run` does NOT re-run flutter create / pod / entitlements every
# time. (Re-touching macos/*.entitlements on every build makes Xcode fail with
# "Entitlements file was modified during the build".)
MOBILE_SETUP_STAMP := mobile/.dart_tool/chartplotter-setup.stamp
MOBILE_SETUP_DEPS := mobile/pubspec.yaml mobile/.fvmrc \
	mobile/tool/ci-android-appauth.sh mobile/tool/ios-appauth.sh \
	mobile/tool/macos-entitlements.sh

# Force the platform setup to re-run: use after deleting the generated
# android/ ios/ macos/ folders (make can't detect those on its own).
mobile-setup:
	rm -f $(MOBILE_SETUP_STAMP)
	$(MAKE) $(MOBILE_SETUP_STAMP)

# The actual setup: install Flutter/CocoaPods if missing, generate the
# gitignored platform folders, inject the appauth/plist/entitlements config,
# and resolve packages. brew installs the latest stable; for the exact version
# pinned in mobile/.fvmrc use FVM (make mobile-setup FLUTTER="fvm flutter").
$(MOBILE_SETUP_STAMP): $(MOBILE_SETUP_DEPS)
	command -v flutter >/dev/null 2>&1 || brew install --cask flutter
	command -v pod >/dev/null 2>&1 || brew install cocoapods
	cd mobile && \
		if [ ! -d ios ] || [ ! -d android ] || [ ! -d macos ]; then $(FLUTTER) create --org $(MOBILE_ORG) . ; fi && \
		bash tool/ci-android-appauth.sh && bash tool/ios-appauth.sh && bash tool/macos-entitlements.sh && \
		$(FLUTTER) pub get
	@mkdir -p $(@D) && touch $@

# Run on a connected device/emulator (sets up first, only when inputs changed).
# MODE=release runs without the Dart VM service — faster, and it sidesteps the
# debug-mode local-network handshake that can hang on a physical iPhone
# ("Dart VM Service was not discovered"). Use debug (default) for hot reload.
mobile-run: $(MOBILE_SETUP_STAMP)
	cd mobile && $(FLUTTER) run $(if $(MODE),--$(MODE)) $(MOBILE_DEVICE) $(MOBILE_DART_DEFINES)

# Build a debug APK (mirrors CI's build-android job).
mobile-apk: $(MOBILE_SETUP_STAMP)
	cd mobile && $(FLUTTER) build apk --debug $(MOBILE_DART_DEFINES)

# Static analysis + unit tests (mirror CI's analyze job). No platform folders
# needed, so these skip mobile-setup.
mobile-analyze:
	cd mobile && $(FLUTTER) analyze
mobile-test:
	cd mobile && $(FLUTTER) test
