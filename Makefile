
ifneq (,$(wildcard test.make))
	include test.make
    export $(shell sed 's/=.*//' test.make)
endif

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

bin/viamchartplottermodule: bin *.go cmd/module/*.go *.mod Makefile dist/index.html
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
		go test -run TestRenderZip10024 -v ./osmtiler -timeout 5m
	$(OPEN) $(OSM_TILES_OUT)/index.html
